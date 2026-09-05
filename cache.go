package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The scan cache exists because a scan re-derives facts that provably cannot
// have changed. A snapshot subvolume is read-only: once walked, its contents
// are fixed for as long as it exists. The expensive half of a scan - walking
// every file in every snapshot, then reading extent maps for the largest of
// them - is therefore pure function of an immutable input, and can be recorded.
//
// What is *not* cached is the live filesystem side. Every scan re-stats the
// live path of every candidate, so deleting or editing files outside this tool
// works exactly as it did before: a file you delete by hand shows up as a new
// candidate on the next scan, cache or no cache.
//
// Correctness rests on the key. An entry is keyed by the subvolume's UUID and
// its CTransID, the transid of the last change to its tree. Modifying a
// snapshot - including this tool's own purge, which clears the read-only flag
// to unlink - moves CTransID, so the old entry simply stops matching. The key
// self-invalidates rather than relying on us to remember to invalidate it.
//
// And the cache cannot make a deletion wrong even if it were stale: purge
// re-validates every target's inode, size and mtime against the filesystem
// immediately before unlinking it (see BuildPlan and unlinkAt in purge.go).
// A stale entry can only produce a wrong report.

const (
	// cacheSchema guards the on-disk format. A mismatch rebuilds rather than
	// misreads, exactly as stateVersion does for the scan state file.
	cacheSchema = 1

	// DefaultCacheFloor is the size threshold manifests are recorded at,
	// independent of the scan's own --min-size.
	//
	// The walk visits every file whatever the threshold; --min-size only gates
	// which ones are reported. Recording at a floor below the usual --min-size
	// therefore costs manifest bytes but no walk time, and it means a later
	// scan at a smaller threshold still hits the cache instead of rewalking.
	DefaultCacheFloor = 1 << 20 // 1 MiB

	bucketMeta     = "meta"
	bucketManifest = "manifest"
	bucketMeasure  = "measure"

	metaSchema   = "schema"
	metaLastScan = "last_scan"

	// measureMaxAge bounds the measurement bucket on filesystems whose
	// snapshots churn faster than GC can notice.
	measureMaxAge = 90 * 24 * time.Hour
)

// manifestEntry is one file recorded inside one snapshot.
type manifestEntry struct {
	Rel     string
	Ino     uint64
	Size    uint64
	MtimeNs int64
}

// manifestBlob is one snapshot's walk result.
type manifestBlob struct {
	// WalkedMinSize is the floor this manifest was collected at. It is reused
	// only for a scan whose --min-size is at or above it; below it the manifest
	// would be missing rows the scan needs.
	WalkedMinSize uint64
	// Complete is false when the walk skipped an unreadable directory. Those
	// are never stored: a manifest with holes would silently under-report.
	Complete bool
	WalkedAt time.Time
	Root     string
	Entries  []manifestEntry
}

// measureBlob is one candidate's extent-union measurement.
type measureBlob struct {
	Usage      SetUsage
	MeasuredAt time.Time
	// Subvols records which snapshots this measurement drew extents from, so
	// GC can drop it when one of them is gone. The key itself is a hash and
	// cannot be taken apart.
	Subvols []string
}

// CacheStats counts what a run got out of the cache.
type CacheStats struct {
	ManifestHits   atomic.Int64
	ManifestMisses atomic.Int64
	MeasureHits    atomic.Int64
	MeasureMisses  atomic.Int64
}

// Cache is the on-disk scan cache. A nil *Cache is valid and does nothing,
// which is how --no-cache and every failure to open one are handled: losing the
// cache must never cost more than speed.
type Cache struct {
	db       *bolt.DB
	path     string
	readOnly bool
	refresh  bool // ignore existing entries, but still write fresh ones
	Stats    CacheStats
}

// CacheOptions controls how the cache is opened and used.
type CacheOptions struct {
	Disabled bool
	Refresh  bool
}

// OpenCache opens (or creates) the cache in the state directory.
//
// Writability is decided by trying, not by inspecting the effective uid:
// ResolveDirs has already picked a state directory this process can write to,
// which for an unprivileged run is under the user's home rather than
// /var/lib. Read-only is the fallback for the cases that are left - a cache
// another process currently holds the lock on, or one on a read-only mount.
//
// Every failure here is soft: the caller gets a nil cache and a note in the
// log, and the scan runs exactly as it did before caching existed.
func OpenCache(dirs Dirs, opts CacheOptions) *Cache {
	if opts.Disabled {
		Infof("cache", "disabled by request")
		return nil
	}
	path := dirs.CacheFile()

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	readOnly := false
	if err != nil {
		if _, serr := os.Stat(path); serr != nil {
			Warnf("cache", "cannot create %s (%v); continuing without a cache", path, err)
			return nil
		}
		// It exists but will not open for writing: another process holds the
		// flock, or the filesystem is read-only. Reading it is still worth
		// having, since that is where the speed-up comes from.
		Infof("cache", "cannot open %s for writing (%v); opening read-only", path, err)
		db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second, ReadOnly: true})
		if err != nil {
			Warnf("cache", "cannot open %s at all (%v); continuing without a cache", path, err)
			return nil
		}
		readOnly = true
	}

	c := &Cache{db: db, path: path, readOnly: readOnly, refresh: opts.Refresh}
	if err := c.init(); err != nil {
		Warnf("cache", "cannot initialise %s (%v); continuing without a cache", path, err)
		db.Close()
		return nil
	}
	Infof("cache", "open at %s (schema %d, read-only=%v, refresh=%v)", path, cacheSchema, readOnly, opts.Refresh)
	return c
}

// init creates the buckets and enforces the schema version, rebuilding from
// scratch when an older format is found.
func (c *Cache) init() error {
	if c.readOnly {
		return c.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(bucketMeta))
			if b == nil || !bytes.Equal(b.Get([]byte(metaSchema)), schemaValue()) {
				return fmt.Errorf("schema mismatch and cannot rebuild read-only")
			}
			return nil
		})
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(bucketMeta))
		if meta != nil && !bytes.Equal(meta.Get([]byte(metaSchema)), schemaValue()) {
			Infof("cache", "schema changed, discarding the old cache")
			for _, n := range []string{bucketMeta, bucketManifest, bucketMeasure} {
				if tx.Bucket([]byte(n)) != nil {
					if err := tx.DeleteBucket([]byte(n)); err != nil {
						return err
					}
				}
			}
		}
		for _, n := range []string{bucketMeta, bucketManifest, bucketMeasure} {
			if _, err := tx.CreateBucketIfNotExists([]byte(n)); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte(bucketMeta)).Put([]byte(metaSchema), schemaValue())
	})
}

func schemaValue() []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], cacheSchema)
	return b[:]
}

// Close releases the cache. Safe on a nil cache.
func (c *Cache) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Path returns the cache file location, or "" when there is no cache.
func (c *Cache) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// subvolIsReadOnly is indirected through a variable so the cache's reuse logic
// can be tested without a real btrfs subvolume to hand.
var subvolIsReadOnly = IsReadOnlySubvol

// manifestKey identifies one immutable state of one snapshot on one
// filesystem. CTransID is what makes it self-invalidating.
func manifestKey(fsID string, s Snapshot) []byte {
	return fmt.Appendf(nil, "%s/%s/%016x", fsID, s.UUID, s.CTransID)
}

// manifestPrefix matches every recorded state of one snapshot.
func manifestPrefix(fsID, subvolUUID string) []byte {
	return fmt.Appendf(nil, "%s/%s/", fsID, subvolUUID)
}

// Manifest returns a snapshot's cached walk result, if one is usable for a scan
// at the given threshold.
func (c *Cache) Manifest(fsID string, s Snapshot, minSize uint64) (*manifestBlob, bool) {
	if c == nil || c.db == nil || c.refresh || !s.Cacheable() {
		if c != nil {
			c.Stats.ManifestMisses.Add(1)
			Count("cache.manifest_miss", 1)
		}
		return nil, false
	}

	// Beyond the key: a snapshot that is no longer read-only is not trustworthy
	// as an immutable input, whatever CTransID says. That covers someone
	// clearing the flag by hand to edit inside a snapshot.
	if ro, err := subvolIsReadOnly(s.Root); err != nil || !ro {
		Debugf("cache", "snapshot %s is not read-only (err=%v); ignoring any cached manifest", s.ID, err)
		c.Stats.ManifestMisses.Add(1)
		Count("cache.manifest_miss", 1)
		return nil, false
	}

	var blob *manifestBlob
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketManifest))
		if b == nil {
			return nil
		}
		raw := b.Get(manifestKey(fsID, s))
		if raw == nil {
			return nil
		}
		m, err := decodeManifest(raw)
		if err != nil {
			return err
		}
		blob = m
		return nil
	})
	if err != nil {
		Warnf("cache", "unreadable manifest for %s: %v", s.ID, err)
		blob = nil
	}
	if blob == nil || blob.WalkedMinSize > minSize {
		if blob != nil {
			Debugf("cache", "snapshot %s cached at floor %s, scan wants %s; rewalking",
				s.ID, FormatBytes(blob.WalkedMinSize), FormatBytes(minSize))
		}
		c.Stats.ManifestMisses.Add(1)
		Count("cache.manifest_miss", 1)
		return nil, false
	}
	c.Stats.ManifestHits.Add(1)
	Count("cache.manifest_hit", 1)
	Debugf("cache", "snapshot %s: reused manifest of %d entries walked %s",
		s.ID, len(blob.Entries), blob.WalkedAt.Format(time.RFC3339))
	return blob, true
}

// PutManifest records a snapshot's walk result. Incomplete walks are dropped
// rather than stored.
func (c *Cache) PutManifest(fsID string, s Snapshot, blob *manifestBlob) {
	if c == nil || c.db == nil || c.readOnly || !s.Cacheable() {
		return
	}
	if !blob.Complete {
		Debugf("cache", "snapshot %s walk was incomplete; not cached", s.ID)
		return
	}
	raw, err := encodeManifest(blob)
	if err != nil {
		Warnf("cache", "cannot encode manifest for %s: %v", s.ID, err)
		return
	}
	err = c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketManifest))
		if err != nil {
			return err
		}
		return b.Put(manifestKey(fsID, s), raw)
	})
	if err != nil {
		Warnf("cache", "cannot store manifest for %s: %v", s.ID, err)
		return
	}
	Debugf("cache", "snapshot %s: cached %d entries (%s compressed, floor %s)",
		s.ID, len(blob.Entries), FormatBytes(uint64(len(raw))), FormatBytes(blob.WalkedMinSize))
	Count("cache.manifest_stored", 1)
}

// DropSnapshot removes every recorded state of one snapshot. CTransID already
// makes a modified snapshot miss; this is the belt to that braces, called after
// a purge has unlinked inside it.
func (c *Cache) DropSnapshot(fsID, subvolUUID string) {
	if c == nil || c.db == nil || c.readOnly || subvolUUID == "" {
		return
	}
	prefix := manifestPrefix(fsID, subvolUUID)
	err := c.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucketManifest)); b != nil {
			cur := b.Cursor()
			for k, _ := cur.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cur.Next() {
				if err := cur.Delete(); err != nil {
					return err
				}
			}
		}
		return dropMeasuresReferencing(tx, map[string]bool{subvolUUID: true})
	})
	if err != nil {
		Warnf("cache", "cannot drop cache entries for subvolume %s: %v", subvolUUID, err)
		return
	}
	Infof("cache", "dropped cached entries for subvolume %s", subvolUUID)
}

// dropMeasuresReferencing deletes every measurement that drew extents from one
// of the given subvolumes. The key is a hash and cannot be taken apart, which
// is why each entry records the subvolumes it covers.
func dropMeasuresReferencing(tx *bolt.Tx, subvols map[string]bool) error {
	b := tx.Bucket([]byte(bucketMeasure))
	if b == nil {
		return nil
	}
	cur := b.Cursor()
	for k, v := cur.First(); k != nil; k, v = cur.Next() {
		var m measureBlob
		if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&m); err != nil {
			continue
		}
		for _, u := range m.Subvols {
			if subvols[u] {
				if err := cur.Delete(); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// measureKey hashes the set of physical files a measurement covers. Every
// member is immutable, so the extent union is a function of the set alone.
// CTransID is folded in so a modified snapshot invalidates its measurements
// along with its manifest.
func measureKey(copies []Copy) ([]byte, []string, bool) {
	parts := make([]string, 0, len(copies))
	seen := map[string]bool{}
	var subvols []string
	for _, cp := range copies {
		if cp.SnapshotUUID == "" || cp.SnapshotCTransID == 0 {
			return nil, nil, false // unidentifiable snapshot: do not cache
		}
		parts = append(parts, fmt.Sprintf("%s:%016x:%d", cp.SnapshotUUID, cp.SnapshotCTransID, cp.Ino))
		if !seen[cp.SnapshotUUID] {
			seen[cp.SnapshotUUID] = true
			subvols = append(subvols, cp.SnapshotUUID)
		}
	}
	sort.Strings(parts)
	sort.Strings(subvols)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return sum[:], subvols, true
}

// Usage returns a cached extent-union measurement for a set of copies.
func (c *Cache) Usage(copies []Copy) (SetUsage, bool) {
	if c == nil || c.db == nil || c.refresh {
		if c != nil {
			c.Stats.MeasureMisses.Add(1)
		}
		return SetUsage{}, false
	}
	key, _, ok := measureKey(copies)
	if !ok {
		c.Stats.MeasureMisses.Add(1)
		return SetUsage{}, false
	}
	var blob *measureBlob
	_ = c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketMeasure))
		if b == nil {
			return nil
		}
		raw := b.Get(key)
		if raw == nil {
			return nil
		}
		var m measureBlob
		if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil {
			return nil
		}
		blob = &m
		return nil
	})
	if blob == nil {
		c.Stats.MeasureMisses.Add(1)
		Count("cache.measure_miss", 1)
		return SetUsage{}, false
	}
	c.Stats.MeasureHits.Add(1)
	Count("cache.measure_hit", 1)
	return blob.Usage, true
}

// PutUsage records an extent-union measurement.
func (c *Cache) PutUsage(copies []Copy, u SetUsage) {
	if c == nil || c.db == nil || c.readOnly || u.Method == MethodNone {
		return
	}
	key, subvols, ok := measureKey(copies)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(measureBlob{
		Usage: u, MeasuredAt: time.Now(), Subvols: subvols,
	}); err != nil {
		return
	}
	err := c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketMeasure))
		if err != nil {
			return err
		}
		return b.Put(key, buf.Bytes())
	})
	if err != nil {
		Warnf("cache", "cannot store measurement: %v", err)
	}
}

// RecordScan stores a one-line summary of the run so `cache status` can report
// what the cache actually saved.
func (c *Cache) RecordScan() {
	if c == nil || c.db == nil || c.readOnly {
		return
	}
	var buf bytes.Buffer
	_ = gob.NewEncoder(&buf).Encode(lastScanRecord{
		At:             time.Now(),
		ManifestHits:   c.Stats.ManifestHits.Load(),
		ManifestMisses: c.Stats.ManifestMisses.Load(),
		MeasureHits:    c.Stats.MeasureHits.Load(),
		MeasureMisses:  c.Stats.MeasureMisses.Load(),
	})
	_ = c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketMeta))
		if err != nil {
			return err
		}
		return b.Put([]byte(metaLastScan), buf.Bytes())
	})
}

type lastScanRecord struct {
	At                           time.Time
	ManifestHits, ManifestMisses int64
	MeasureHits, MeasureMisses   int64
}

// GC drops entries for snapshots that no longer exist, plus measurements old
// enough that nothing is going to ask for them again.
func (c *Cache) GC(pairs []Pair) (int, error) {
	if c == nil || c.db == nil || c.readOnly {
		return 0, nil
	}
	live := map[string]bool{}
	for _, p := range pairs {
		for _, s := range p.Snapshots {
			if s.UUID != "" {
				live[s.UUID] = true
			}
		}
	}
	if len(live) == 0 {
		// Detection found nothing this run; that is no evidence the recorded
		// snapshots are gone, so removing anything here would be guessing.
		Debugf("cache", "no live snapshots identified; skipping GC")
		return 0, nil
	}

	var removed int
	dead := map[string]bool{}
	err := c.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucketManifest)); b != nil {
			cur := b.Cursor()
			for k, _ := cur.First(); k != nil; k, _ = cur.Next() {
				parts := strings.Split(string(k), "/")
				if len(parts) != 3 {
					continue
				}
				if !live[parts[1]] {
					dead[parts[1]] = true
					if err := cur.Delete(); err != nil {
						return err
					}
					removed++
				}
			}
		}
		if b := tx.Bucket([]byte(bucketMeasure)); b != nil {
			cutoff := time.Now().Add(-measureMaxAge)
			cur := b.Cursor()
			for k, v := cur.First(); k != nil; k, v = cur.Next() {
				var m measureBlob
				if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&m); err != nil {
					if err := cur.Delete(); err != nil {
						return err
					}
					removed++
					continue
				}
				drop := m.MeasuredAt.Before(cutoff)
				for _, u := range m.Subvols {
					if !live[u] {
						drop = true
						break
					}
				}
				if drop {
					if err := cur.Delete(); err != nil {
						return err
					}
					removed++
				}
			}
		}
		return nil
	})
	if err != nil {
		return removed, err
	}
	if removed > 0 {
		Infof("cache", "GC removed %d entry/entries (%d snapshot(s) no longer present)", removed, len(dead))
	}
	return removed, nil
}

// Clear empties the cache without removing the file.
func (c *Cache) Clear() error {
	if c == nil || c.db == nil {
		return fmt.Errorf("no cache is open")
	}
	if c.readOnly {
		return fmt.Errorf("cache is open read-only")
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		for _, n := range []string{bucketManifest, bucketMeasure} {
			if tx.Bucket([]byte(n)) != nil {
				if err := tx.DeleteBucket([]byte(n)); err != nil {
					return err
				}
			}
			if _, err := tx.CreateBucketIfNotExists([]byte(n)); err != nil {
				return err
			}
		}
		return nil
	})
}

// CacheStatus is what `cache status` reports.
type CacheStatus struct {
	Path         string
	Bytes        uint64
	Manifests    int
	ManifestRows int
	Measures     int
	Oldest       time.Time
	Newest       time.Time
	LastScan     *lastScanRecord
	Subvols      []string
}

// Status summarises the cache contents.
func (c *Cache) Status() (*CacheStatus, error) {
	if c == nil || c.db == nil {
		return nil, fmt.Errorf("no cache is open")
	}
	st := &CacheStatus{Path: c.path}
	if fi, err := os.Stat(c.path); err == nil {
		st.Bytes = uint64(fi.Size())
	}
	seen := map[string]bool{}
	err := c.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucketManifest)); b != nil {
			return_err := b.ForEach(func(k, v []byte) error {
				st.Manifests++
				if parts := strings.Split(string(k), "/"); len(parts) == 3 && !seen[parts[1]] {
					seen[parts[1]] = true
					st.Subvols = append(st.Subvols, parts[1])
				}
				m, err := decodeManifest(v)
				if err != nil {
					return nil
				}
				st.ManifestRows += len(m.Entries)
				if st.Oldest.IsZero() || m.WalkedAt.Before(st.Oldest) {
					st.Oldest = m.WalkedAt
				}
				if m.WalkedAt.After(st.Newest) {
					st.Newest = m.WalkedAt
				}
				return nil
			})
			if return_err != nil {
				return return_err
			}
		}
		if b := tx.Bucket([]byte(bucketMeasure)); b != nil {
			st.Measures = b.Stats().KeyN
		}
		if b := tx.Bucket([]byte(bucketMeta)); b != nil {
			if raw := b.Get([]byte(metaLastScan)); raw != nil {
				var r lastScanRecord
				if gob.NewDecoder(bytes.NewReader(raw)).Decode(&r) == nil {
					st.LastScan = &r
				}
			}
		}
		return nil
	})
	sort.Strings(st.Subvols)
	return st, err
}

// Manifests are gzipped because they are long runs of similar path strings and
// compress by roughly an order of magnitude, which keeps the cache file small
// enough to be uninteresting even with a low floor.
func encodeManifest(b *manifestBlob) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if err := gob.NewEncoder(zw).Encode(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeManifest(raw []byte) (*manifestBlob, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var b manifestBlob
	if err := gob.NewDecoder(zr).Decode(&b); err != nil && err != io.EOF {
		return nil, err
	}
	return &b, nil
}

// CacheFile is where the cache lives, beside the scan state.
func (d Dirs) CacheFile() string { return filepath.Join(d.State, "cache.db") }

// shortUUID trims a subvolume UUID for display.
func shortUUID(u string) string {
	if len(u) > 8 {
		return u[:8]
	}
	return u
}
