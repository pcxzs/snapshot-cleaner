package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// readOnlySubvols makes every snapshot root look like a read-only subvolume, so
// the cache's reuse path can be exercised without a btrfs filesystem.
func readOnlySubvols(t *testing.T) {
	t.Helper()
	prev := subvolIsReadOnly
	subvolIsReadOnly = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { subvolIsReadOnly = prev })
}

func testDirs(t *testing.T) Dirs {
	t.Helper()
	return Dirs{State: t.TempDir(), Runtime: t.TempDir()}
}

func testSnapshot(id string, ctransid uint64) Snapshot {
	return Snapshot{ID: id, Root: "/snap/" + id, UUID: "uuid-" + id, CTransID: ctransid, ReadOnly: true}
}

func sampleManifest(floor uint64) *manifestBlob {
	return &manifestBlob{
		WalkedMinSize: floor,
		Complete:      true,
		WalkedAt:      time.Now().Truncate(time.Second),
		Root:          "/snap/a",
		Entries: []manifestEntry{
			{Rel: "big.iso", Ino: 300, Size: 4 << 30, MtimeNs: 1234567890},
			{Rel: "sub/mid.bin", Ino: 301, Size: 100 << 20, MtimeNs: 987654321},
		},
	}
}

// The cache must survive a round trip byte for byte: a manifest that comes back
// altered would produce a purge plan that fails validation at best.
func TestManifestRoundTrip(t *testing.T) {
	raw, err := encodeManifest(sampleManifest(1 << 20))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := sampleManifest(1 << 20)
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("got %d entries, want %d", len(got.Entries), len(want.Entries))
	}
	for i := range want.Entries {
		if got.Entries[i] != want.Entries[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got.Entries[i], want.Entries[i])
		}
	}
	if got.WalkedMinSize != want.WalkedMinSize || !got.Complete {
		t.Errorf("metadata lost: %+v", got)
	}
}

// Reuse is governed by the floor the manifest was recorded at, not by the
// threshold of the scan that recorded it. A scan wanting smaller files than the
// manifest holds must miss, or it would silently under-report.
func TestManifestFloorGovernsReuse(t *testing.T) {
	readOnlySubvols(t)
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	if c == nil {
		t.Fatal("cache did not open")
	}
	defer c.Close()

	snap := testSnapshot("a", 42)
	c.PutManifest("fs1", snap, sampleManifest(50<<20))

	if _, ok := c.Manifest("fs1", snap, 10<<20); ok {
		t.Error("a scan below the recorded floor must miss, not reuse a partial list")
	}
	if _, ok := c.Manifest("fs1", snap, 50<<20); !ok {
		t.Error("a scan at the recorded floor must reuse it")
	}
	if _, ok := c.Manifest("fs1", snap, 100<<20); !ok {
		t.Error("a scan above the recorded floor must reuse it")
	}
}

// The whole point of the cache: a snapshot walked once is not walked again.
func TestManifestHitReturnsWhatWasStored(t *testing.T) {
	readOnlySubvols(t)
	c := OpenCache(testDirs(t), CacheOptions{})
	defer c.Close()

	snap := testSnapshot("a", 42)
	c.PutManifest("fs1", snap, sampleManifest(1<<20))

	got, ok := c.Manifest("fs1", snap, 1<<20)
	if !ok {
		t.Fatal("an unchanged snapshot must hit")
	}
	if len(got.Entries) != 2 || got.Entries[0].Rel != "big.iso" {
		t.Fatalf("got %+v", got.Entries)
	}
	if c.Stats.ManifestHits.Load() != 1 {
		t.Errorf("hit was not counted: %d", c.Stats.ManifestHits.Load())
	}
}

// Modifying a snapshot must invalidate it even though its UUID is unchanged.
func TestModifiedSnapshotMisses(t *testing.T) {
	readOnlySubvols(t)
	c := OpenCache(testDirs(t), CacheOptions{})
	defer c.Close()

	c.PutManifest("fs1", testSnapshot("a", 42), sampleManifest(1<<20))
	if _, ok := c.Manifest("fs1", testSnapshot("a", 43), 1<<20); ok {
		t.Error("a snapshot whose tree changed must not reuse its old listing")
	}
}

// A snapshot found writable is not a trustworthy immutable input whatever its
// transid says, so it is rewalked. This is what catches someone clearing the
// read-only flag by hand to delete inside a snapshot.
func TestWritableSnapshotIsDistrusted(t *testing.T) {
	readOnlySubvols(t)
	c := OpenCache(testDirs(t), CacheOptions{})
	defer c.Close()

	snap := testSnapshot("a", 42)
	c.PutManifest("fs1", snap, sampleManifest(1<<20))
	if _, ok := c.Manifest("fs1", snap, 1<<20); !ok {
		t.Fatal("precondition: a read-only snapshot should hit")
	}

	subvolIsReadOnly = func(string) (bool, error) { return false, nil }
	if _, ok := c.Manifest("fs1", snap, 1<<20); ok {
		t.Error("a snapshot that is no longer read-only must be rewalked")
	}
}

// CTransID is the whole safety argument: a snapshot that changed must not
// resolve to what it held before.
func TestManifestKeyChangesWithCTransID(t *testing.T) {
	before := manifestKey("fs1", testSnapshot("a", 42))
	after := manifestKey("fs1", testSnapshot("a", 43))
	if string(before) == string(after) {
		t.Fatal("a modified snapshot must produce a different key")
	}
	other := manifestKey("fs2", testSnapshot("a", 42))
	if string(before) == string(other) {
		t.Fatal("the same subvolume UUID on another filesystem must not collide")
	}
}

// A snapshot the kernel would not identify has no safe key, so it must simply
// never be cached rather than be keyed on something weaker.
func TestUnidentifiedSnapshotIsNeverCached(t *testing.T) {
	readOnlySubvols(t)
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	snap := Snapshot{ID: "x", Root: t.TempDir()} // no UUID, no CTransID
	if snap.Cacheable() {
		t.Fatal("a snapshot without identity must not report itself cacheable")
	}
	c.PutManifest("fs1", snap, sampleManifest(1<<20))
	if _, ok := c.Manifest("fs1", snap, 1<<20); ok {
		t.Error("an unidentified snapshot must never produce a cache hit")
	}
}

// A walk that could not read part of the tree describes an incomplete snapshot.
// Storing it would make the omission permanent.
func TestIncompleteManifestIsNotStored(t *testing.T) {
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	snap := testSnapshot("a", 1)
	snap.Root = t.TempDir()
	blob := sampleManifest(1 << 20)
	blob.Complete = false
	c.PutManifest("fs1", snap, blob)

	st, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Manifests != 0 {
		t.Errorf("stored %d incomplete manifest(s), want 0", st.Manifests)
	}
}

// Measurements are keyed on the physical files, not their paths: snapshot
// directories get recycled and inode numbers get reused, so a path-based key
// would eventually return one file's size for another.
func TestMeasureKeyIdentifiesPhysicalFiles(t *testing.T) {
	a := []Copy{{Path: "/snap/1/x", SnapshotUUID: "u1", SnapshotCTransID: 10, Ino: 300}}
	sameFileMovedPath := []Copy{{Path: "/elsewhere/x", SnapshotUUID: "u1", SnapshotCTransID: 10, Ino: 300}}
	recycledSnapshot := []Copy{{Path: "/snap/1/x", SnapshotUUID: "u1", SnapshotCTransID: 11, Ino: 300}}

	k1, _, ok := measureKey(a)
	if !ok {
		t.Fatal("a fully identified copy set must be keyable")
	}
	k2, _, _ := measureKey(sameFileMovedPath)
	if string(k1) != string(k2) {
		t.Error("the same physical file reached by another path must hit the same key")
	}
	k3, _, _ := measureKey(recycledSnapshot)
	if string(k1) == string(k3) {
		t.Error("a changed snapshot must not reuse its old measurements")
	}
	if _, _, ok := measureKey([]Copy{{Path: "/snap/1/x", Ino: 300}}); ok {
		t.Error("a copy without snapshot identity must not be keyable at all")
	}
}

// Order is not identity: the same set of copies in a different order is the
// same set of extents.
func TestMeasureKeyIsOrderIndependent(t *testing.T) {
	a := []Copy{
		{SnapshotUUID: "u1", SnapshotCTransID: 1, Ino: 300},
		{SnapshotUUID: "u2", SnapshotCTransID: 2, Ino: 301},
	}
	b := []Copy{a[1], a[0]}
	ka, _, _ := measureKey(a)
	kb, _, _ := measureKey(b)
	if string(ka) != string(kb) {
		t.Fatal("copy order must not change the key")
	}
}

func TestUsageRoundTrip(t *testing.T) {
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	copies := []Copy{{SnapshotUUID: "u1", SnapshotCTransID: 1, Ino: 300}}
	want := SetUsage{Bytes: 1 << 30, Method: MethodTreeSearch, Exact: true, Compressed: true}
	c.PutUsage(copies, want)

	got, ok := c.Usage(copies)
	if !ok {
		t.Fatal("a stored measurement must be readable back")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	// A failed measurement is not a fact worth remembering.
	c.PutUsage([]Copy{{SnapshotUUID: "u9", SnapshotCTransID: 1, Ino: 1}}, SetUsage{Method: MethodNone})
	if _, ok := c.Usage([]Copy{{SnapshotUUID: "u9", SnapshotCTransID: 1, Ino: 1}}); ok {
		t.Error("an unmeasurable set must not be cached as measured")
	}
}

// --refresh must rewalk and then leave the cache usable, not merely bypass it.
func TestRefreshIgnoresButRewritesEntries(t *testing.T) {
	dirs := testDirs(t)
	snap := testSnapshot("a", 1)
	snap.Root = t.TempDir()

	c := OpenCache(dirs, CacheOptions{})
	c.PutManifest("fs1", snap, sampleManifest(1<<20))
	c.Close()

	r := OpenCache(dirs, CacheOptions{Refresh: true})
	defer r.Close()
	if _, ok := r.Manifest("fs1", snap, 1<<20); ok {
		t.Error("--refresh must not reuse an existing entry")
	}
	r.PutManifest("fs1", snap, sampleManifest(1<<20))
	st, _ := r.Status()
	if st.Manifests != 1 {
		t.Errorf("--refresh must still write: got %d manifest(s)", st.Manifests)
	}
}

// GC drops what belongs to snapshots the manager has rotated away, and keeps
// what is still live.
func TestGCDropsRotatedSnapshots(t *testing.T) {
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	live := testSnapshot("live", 1)
	live.Root = t.TempDir()
	gone := testSnapshot("gone", 1)
	gone.Root = t.TempDir()
	c.PutManifest("fs1", live, sampleManifest(1<<20))
	c.PutManifest("fs1", gone, sampleManifest(1<<20))
	c.PutUsage([]Copy{{SnapshotUUID: gone.UUID, SnapshotCTransID: 1, Ino: 5}},
		SetUsage{Bytes: 1, Method: MethodFiemap})

	n, err := c.GC([]Pair{{Snapshots: []Snapshot{live}}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d entries, want 2 (one manifest and its measurement)", n)
	}
	st, _ := c.Status()
	if st.Manifests != 1 {
		t.Errorf("kept %d manifest(s), want the live one only", st.Manifests)
	}
}

// Detection coming up empty is not evidence that the recorded snapshots are
// gone, and must not be treated as licence to empty the cache.
func TestGCKeepsEverythingWhenDetectionFindsNothing(t *testing.T) {
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	snap := testSnapshot("a", 1)
	snap.Root = t.TempDir()
	c.PutManifest("fs1", snap, sampleManifest(1<<20))

	if n, err := c.GC(nil); err != nil || n != 0 {
		t.Fatalf("GC with no detected pairs removed %d entries (err %v), want 0", n, err)
	}
}

// A purge is the one modification this tool makes to a snapshot, and it must
// take that snapshot's cached facts with it.
func TestDropSnapshotRemovesManifestsAndMeasurements(t *testing.T) {
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	snap := testSnapshot("a", 7)
	snap.Root = t.TempDir()
	c.PutManifest("fs1", snap, sampleManifest(1<<20))
	copies := []Copy{{SnapshotUUID: snap.UUID, SnapshotCTransID: 7, Ino: 300}}
	c.PutUsage(copies, SetUsage{Bytes: 1 << 20, Method: MethodTreeSearch, Exact: true})

	c.DropSnapshot("fs1", snap.UUID)

	st, _ := c.Status()
	if st.Manifests != 0 || st.Measures != 0 {
		t.Errorf("after a purge: %d manifest(s) and %d measurement(s) left, want none",
			st.Manifests, st.Measures)
	}
}

// An older cache format must be discarded rather than misread, exactly as the
// scan state file refuses a version it does not understand.
func TestSchemaMismatchRebuilds(t *testing.T) {
	dirs := testDirs(t)
	snap := testSnapshot("a", 1)
	snap.Root = t.TempDir()

	c := OpenCache(dirs, CacheOptions{})
	c.PutManifest("fs1", snap, sampleManifest(1<<20))
	c.Close()

	// Rewrite the recorded schema to something this build does not know.
	db, err := bolt.Open(dirs.CacheFile(), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketMeta)).Put([]byte(metaSchema), []byte("999xxxxx"))
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	c2 := OpenCache(dirs, CacheOptions{})
	if c2 == nil {
		t.Fatal("a schema mismatch must rebuild, not refuse to open")
	}
	defer c2.Close()
	st, _ := c2.Status()
	if st.Manifests != 0 {
		t.Errorf("kept %d manifest(s) across a schema change, want 0", st.Manifests)
	}
}

// Losing the cache must cost speed and nothing else.
func TestNilCacheIsUsable(t *testing.T) {
	var c *Cache
	if _, ok := c.Manifest("fs", testSnapshot("a", 1), 0); ok {
		t.Error("a nil cache must never report a hit")
	}
	if _, ok := c.Usage(nil); ok {
		t.Error("a nil cache must never report a measurement")
	}
	c.PutManifest("fs", testSnapshot("a", 1), sampleManifest(0))
	c.PutUsage(nil, SetUsage{})
	c.DropSnapshot("fs", "u")
	c.RecordScan()
	if n, err := c.GC(nil); n != 0 || err != nil {
		t.Error("GC on a nil cache must be a no-op")
	}
	if err := c.Close(); err != nil {
		t.Error("closing a nil cache must not error")
	}
}

func TestOpenCacheDisabled(t *testing.T) {
	if c := OpenCache(testDirs(t), CacheOptions{Disabled: true}); c != nil {
		t.Fatal("--no-cache must produce no cache at all")
	}
}

func TestCacheFileLivesBesideTheState(t *testing.T) {
	d := Dirs{State: "/var/lib/snapshot-cleaner"}
	if got, want := d.CacheFile(), filepath.Join(d.State, "cache.db"); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestClearEmptiesButKeepsTheFile(t *testing.T) {
	dirs := testDirs(t)
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	snap := testSnapshot("a", 1)
	snap.Root = t.TempDir()
	c.PutManifest("fs1", snap, sampleManifest(1<<20))
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(); st.Manifests != 0 {
		t.Errorf("clear left %d manifest(s)", st.Manifests)
	}
	if _, err := os.Stat(dirs.CacheFile()); err != nil {
		t.Errorf("clear must empty the cache, not remove it: %v", err)
	}
}
