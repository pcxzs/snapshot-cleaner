package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Kinds of candidate.
const (
	KindDeleted  = "deleted"  // gone from the live tree entirely
	KindReplaced = "replaced" // still present live, but the snapshot holds older content
)

// Copy is one snapshot's copy of a candidate file, fingerprinted so purge can
// confirm nothing changed between scanning and deleting.
type Copy struct {
	SnapshotID string `json:"snapshot_id"`
	Snapshot   string `json:"snapshot_root"`
	Path       string `json:"path"`
	Ino        uint64 `json:"ino"`
	Size       uint64 `json:"size"`
	MtimeNs    int64  `json:"mtime_ns"`

	// Subvolume identity, carried so a cached measurement can be keyed on the
	// physical file rather than on its path. Snapshot directories are recycled
	// by their managers, and inode numbers are reused within a filesystem, so
	// (root path, ino) is not a safe key across a rotation; (uuid, ctransid,
	// ino) is.
	SnapshotUUID     string `json:"snapshot_uuid,omitempty"`
	SnapshotCTransID uint64 `json:"snapshot_ctransid,omitempty"`
}

// Candidate is one file path held by one or more snapshots that is no longer
// backed by identical live content.
type Candidate struct {
	ID       int      `json:"id"`
	Provider string   `json:"provider"`
	Pair     string   `json:"pair"`
	Live     string   `json:"live"`
	RelPath  string   `json:"rel_path"`
	LivePath string   `json:"live_path"`
	Kind     string   `json:"kind"`
	Apparent uint64   `json:"apparent_bytes"`
	Copies   []Copy   `json:"copies"`
	Usage    SetUsage `json:"usage"`
	TotalIn  int      `json:"snapshots_in_pair"`
}

// DefaultWorkers picks the snapshot-walk concurrency.
//
// The walk is bound by btrfs metadata reads rather than by CPU, so it scales
// past the core count on an SSD: measured on a 16-core NVMe machine, walking 15
// snapshots four at a time took 133s, with each wave of four finishing
// simultaneously at ~74s. The cap keeps a spinning disk from being thrashed by
// dozens of concurrent seeks, and --workers overrides either way.
func DefaultWorkers() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 2 {
		n = 2
	}
	return n
}

// WalkMode selects how a snapshot's file list is obtained.
type WalkMode int

const (
	WalkAuto    WalkMode = iota // tree search when permitted, readdir otherwise
	WalkTree                    // force BTRFS_IOC_TREE_SEARCH_V2
	WalkReaddir                 // force the portable readdir walk
)

func (m WalkMode) String() string {
	switch m {
	case WalkTree:
		return "treesearch"
	case WalkReaddir:
		return "readdir"
	default:
		return "auto"
	}
}

// ScanOptions controls a scan.
type ScanOptions struct {
	MinSize         uint64
	IncludeReplaced bool
	Excludes        []string
	CostLimit       int
	Workers         int
	Priority        Priority
	Progress        io.Writer

	// Cache may be nil, which disables caching without any special casing.
	Cache *Cache
	// CacheFloor is the size threshold manifests are recorded at. It is at or
	// below MinSize, so one manifest serves scans at several thresholds.
	CacheFloor uint64
	Walk       WalkMode
}

// Scan walks every snapshot of every pair and returns candidates ranked by the
// bytes that would actually be reclaimed.
func Scan(pairs []Pair, opts ScanOptions) ([]Candidate, error) {
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers()
	}

	if opts.CacheFloor == 0 || opts.CacheFloor > opts.MinSize {
		opts.CacheFloor = opts.MinSize
	}
	opts.Walk = chooseWalk(pairs, opts.Walk)

	Infof("scan", "starting: %d pair(s), min-size=%s workers=%d include-replaced=%v excludes=%v cost-limit=%d priority=%s walk=%s cache-floor=%s",
		len(pairs), FormatBytes(opts.MinSize), opts.Workers, opts.IncludeReplaced, opts.Excludes, opts.CostLimit,
		opts.Priority, opts.Walk, FormatBytes(opts.CacheFloor))

	var all []Candidate
	for _, pair := range pairs {
		started := time.Now()
		cands, err := scanPair(pair, opts)
		if err != nil {
			Errorf("scan", "pair %s failed after %s: %v", pair.Name, time.Since(started).Round(time.Millisecond), err)
			return nil, err
		}
		Infof("scan", "pair %s: %d candidate(s) in %s",
			pair.Name, len(cands), time.Since(started).Round(time.Millisecond))
		all = append(all, cands...)
	}

	// Cost the most promising candidates first; measuring every one would mean
	// reading extent maps for thousands of files to rank a handful.
	sort.Slice(all, func(i, j int) bool { return all[i].Apparent > all[j].Apparent })
	limit := opts.CostLimit
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	Infof("scan", "measuring the %d largest of %d candidate(s)", limit, len(all))
	costStart := time.Now()
	costSet(all[:limit], opts)
	Infof("scan", "measurement finished in %s", time.Since(costStart).Round(time.Millisecond))
	if opts.Cache != nil {
		Infof("scan", "cache: %d/%d snapshot manifest(s) reused, %d/%d measurement(s) reused",
			opts.Cache.Stats.ManifestHits.Load(),
			opts.Cache.Stats.ManifestHits.Load()+opts.Cache.Stats.ManifestMisses.Load(),
			opts.Cache.Stats.MeasureHits.Load(),
			opts.Cache.Stats.MeasureHits.Load()+opts.Cache.Stats.MeasureMisses.Load())
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Usage.Bytes != all[j].Usage.Bytes {
			return all[i].Usage.Bytes > all[j].Usage.Bytes
		}
		return all[i].Apparent > all[j].Apparent
	})
	for i := range all {
		all[i].ID = i + 1
	}
	logCandidates(all, limit)
	return all, nil
}

// found is one file seen inside one snapshot, whether it was just walked or
// replayed from the cache. Holding the recorded fields rather than a Stat_t is
// what lets those two sources be interchangeable.
type found struct {
	snapshot Snapshot
	entry    manifestEntry
}

// chooseWalk settles on one walk implementation for the whole run, so the log
// and `doctor` can state which was used and why instead of it varying per
// snapshot.
func chooseWalk(pairs []Pair, want WalkMode) WalkMode {
	if want != WalkAuto {
		return want
	}
	for _, p := range pairs {
		for _, s := range p.Snapshots {
			if err := TreeWalkSupported(s.Root); err != nil {
				Infof("scan", "tree-search walk unavailable (%v); using the readdir walk", err)
				return WalkReaddir
			}
			return WalkTree
		}
	}
	return WalkReaddir
}

func scanPair(pair Pair, opts ScanOptions) ([]Candidate, error) {
	var (
		mu      sync.Mutex
		byPath  = map[string][]found{}
		scanned atomic.Int64
		wg      sync.WaitGroup
		errOnce sync.Once
		scanErr error
	)

	// The cache is keyed per filesystem, so a snapshot UUID recorded for one
	// filesystem can never be mistaken for the same UUID on another.
	fsID, err := FilesystemID(pair.Live)
	if err != nil {
		Warnf("scan", "pair %s: cannot read filesystem id (%v); not using the cache", pair.Name, err)
	}

	jobs := make(chan Snapshot)
	for w := 0; w < opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Per-thread on Linux, so it has to be done here rather than once
			// in main, and the goroutine must stay on its thread.
			defer opts.Priority.ApplyToWorker()()
			for snap := range jobs {
				snapStart := time.Now()

				var (
					manifest *manifestBlob
					hit      bool
				)
				if fsID != "" {
					manifest, hit = opts.Cache.Manifest(fsID, snap, opts.MinSize)
				}
				if !hit {
					var err error
					manifest, err = collectManifest(snap, opts)
					if err != nil {
						// Record the failure but keep draining the channel: a
						// worker that returns early would leave the sender
						// blocked forever once every worker has bailed out.
						Errorf("walk", "snapshot %s unreadable: %v", snap.ID, err)
						errOnce.Do(func() { scanErr = err })
						continue
					}
					if fsID != "" {
						opts.Cache.PutManifest(fsID, snap, manifest)
					}
					Count("walk.snapshots", 1)
				} else {
					Count("walk.snapshots_cached", 1)
				}

				// Policy is applied here rather than when the manifest was
				// recorded, so one manifest serves scans with different
				// --min-size and --exclude settings.
				local := map[string][]found{}
				var seen int64
				for _, e := range manifest.Entries {
					if e.Size < opts.MinSize {
						continue
					}
					if excluded(e.Rel, filepath.Base(e.Rel), opts.Excludes) {
						continue
					}
					local[e.Rel] = append(local[e.Rel], found{snapshot: snap, entry: e})
					seen++
					Tracef("walk", "%s/%s size=%d ino=%d", snap.ID, e.Rel, e.Size, e.Ino)
					if n := scanned.Add(1); n%20000 == 0 {
						reportProgress(opts.Progress, pair.Name, n)
					}
				}
				Debugf("walk", "snapshot %s (%s): %d file(s) at or above threshold in %s (cached=%v)",
					snap.ID, snap.Root, seen, time.Since(snapStart).Round(time.Millisecond), hit)
				Count("walk.files_over_threshold", seen)

				mu.Lock()
				for rel, fs := range local {
					byPath[rel] = append(byPath[rel], fs...)
				}
				mu.Unlock()
			}
		}()
	}
	for _, s := range pair.Snapshots {
		jobs <- s
	}
	close(jobs)
	wg.Wait()
	if scanErr != nil {
		return nil, scanErr
	}

	var out []Candidate
	for rel, copies := range byPath {
		livePath := filepath.Join(pair.Live, rel)
		var liveSt unix.Stat_t
		liveErr := unix.Lstat(livePath, &liveSt)

		kind := KindDeleted
		if liveErr == nil {
			Tracef("diff", "%s: present live (ino=%d size=%d)", rel, liveSt.Ino, liveSt.Size)
			// Present live: only the snapshot copies that differ from the live
			// file are pinning anything. Identical copies share every extent
			// with the live file and free nothing.
			before := len(copies)
			copies = filterDiffering(copies, &liveSt)
			if len(copies) == 0 {
				Count("diff.identical_to_live", 1)
				Tracef("diff", "%s: all %d snapshot copies identical to live, frees nothing", rel, before)
				continue
			}
			if !opts.IncludeReplaced {
				Count("diff.replaced_skipped", 1)
				Tracef("diff", "%s: %d/%d copies differ from live, skipped (no --include-replaced)",
					rel, len(copies), before)
				continue
			}
			Count("diff.replaced", 1)
			kind = KindReplaced
		}

		cand := Candidate{
			Provider: pair.Provider,
			Pair:     pair.Name,
			Live:     pair.Live,
			RelPath:  rel,
			LivePath: livePath,
			Kind:     kind,
			TotalIn:  len(pair.Snapshots),
			// Stays MethodNone unless costSet measures it, so a candidate
			// beyond --cost-limit renders as unmeasured rather than as zero.
			Usage: SetUsage{Method: MethodNone},
		}
		for _, f := range copies {
			if f.entry.Size > cand.Apparent {
				cand.Apparent = f.entry.Size
			}
			cand.Copies = append(cand.Copies, Copy{
				SnapshotID:       f.snapshot.ID,
				Snapshot:         f.snapshot.Root,
				Path:             filepath.Join(f.snapshot.Root, rel),
				Ino:              f.entry.Ino,
				Size:             f.entry.Size,
				MtimeNs:          f.entry.MtimeNs,
				SnapshotUUID:     f.snapshot.UUID,
				SnapshotCTransID: f.snapshot.CTransID,
			})
		}
		sort.Slice(cand.Copies, func(i, j int) bool { return cand.Copies[i].SnapshotID < cand.Copies[j].SnapshotID })
		if kind == KindDeleted {
			Count("diff.deleted", 1)
		}
		Tracef("diff", "candidate %s kind=%s apparent=%d copies=%d/%d",
			rel, kind, cand.Apparent, len(cand.Copies), cand.TotalIn)
		out = append(out, cand)
	}
	return out, nil
}

// filterDiffering keeps only the snapshot copies whose content differs from the
// live file, identified by inode: a reflinked-but-unchanged copy shares the
// live inode's extents and costs nothing extra.
func filterDiffering(copies []found, live *unix.Stat_t) []found {
	var out []found
	for _, c := range copies {
		if c.entry.Ino == live.Ino && c.entry.Size == uint64(live.Size) && c.entry.MtimeNs == live.Mtim.Nano() {
			continue
		}
		out = append(out, c)
	}
	return out
}

// collectManifest produces one snapshot's file list.
//
// The manifest is deliberately unfiltered: it records every regular file at or
// above the cache floor, with no --exclude applied and no --min-size beyond the
// floor. Policy belongs to the scan that reads it, not to the recording, or one
// invocation's flags would poison the cache for the next.
//
// The one exception is the readdir walk with excludes set. There, pruning
// excluded subtrees is the only way --exclude ever saved any time, so it is
// kept - and the resulting manifest is marked incomplete so it is never cached.
func collectManifest(snap Snapshot, opts ScanOptions) (*manifestBlob, error) {
	blob := &manifestBlob{
		WalkedMinSize: opts.CacheFloor,
		Complete:      true,
		WalkedAt:      time.Now(),
		Root:          snap.Root,
	}

	if opts.Walk == WalkTree {
		entries, err := treeWalkSnapshot(snap.Root, opts.CacheFloor)
		if err == nil {
			blob.Entries = entries
			return blob, nil
		}
		// A tree that cannot be searched now (a snapshot being deleted under
		// us, a kernel that refuses) is not a reason to fail the whole scan.
		Warnf("walk", "tree-search walk of %s failed (%v); falling back to readdir", snap.ID, err)
	}

	prune := opts.Excludes
	if len(prune) > 0 {
		blob.Complete = false
	}
	entries, complete, err := readdirWalk(snap.Root, opts.CacheFloor, prune)
	if err != nil {
		return nil, err
	}
	blob.Entries = entries
	blob.Complete = blob.Complete && complete
	return blob, nil
}

// readdirWalk walks one snapshot subvolume with readdir and lstat, reporting
// regular files at or above floor. It never follows symlinks and never crosses
// into a different device, which keeps it inside the one subvolume.
//
// prune skips matching entries during the walk rather than after it. It is the
// only thing that makes --exclude cheaper than not excluding, and callers that
// want a cacheable result pass nil.
//
// The returned bool is false if any directory could not be read, so the caller
// can tell a complete list from one with holes in it.
func readdirWalk(root string, floor uint64, prune []string) ([]manifestEntry, bool, error) {
	var rootSt unix.Stat_t
	if err := unix.Lstat(root, &rootSt); err != nil {
		return nil, false, fmt.Errorf("stat snapshot root %s: %w", root, err)
	}

	var (
		out      []manifestEntry
		complete = true
	)
	stack := []string{""}
	for len(stack) > 0 {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		dir := filepath.Join(root, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			// An unreadable directory inside a snapshot is not fatal, but it
			// does mean the list is incomplete and must not be cached.
			Debugf("walk", "cannot read %s: %v", dir, err)
			complete = false
			continue
		}
		for _, e := range entries {
			childRel := filepath.Join(rel, e.Name())
			if excluded(childRel, e.Name(), prune) {
				continue
			}
			var st unix.Stat_t
			if err := unix.Lstat(filepath.Join(root, childRel), &st); err != nil {
				complete = false
				continue
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				if st.Dev != rootSt.Dev {
					continue // a nested subvolume or mount, not part of this tree
				}
				stack = append(stack, childRel)
			case unix.S_IFREG:
				if uint64(st.Size) >= floor {
					out = append(out, manifestEntry{
						Rel:     childRel,
						Ino:     st.Ino,
						Size:    uint64(st.Size),
						MtimeNs: st.Mtim.Nano(),
					})
				}
			}
		}
	}
	return out, complete, nil
}

// excluded reports whether rel is filtered out by any --exclude pattern.
//
// A pattern is matched against the path relative to the subvolume root, against
// the file's own name, and against every ancestor directory of the path - both
// the ancestor's full relative path and its bare name. The ancestors matter
// because filepath.Match's * never crosses a /, so "deep/*" alone matches
// "deep/nested" but not "deep/nested/buried.bin", and "deep" matches neither.
// Without the ancestor walk no pattern could exclude a subtree, which is the
// thing people reach for --exclude to do, and it would fail silently.
func excluded(rel, base string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		for dir := filepath.Dir(rel); dir != "." && dir != "/" && dir != ""; dir = filepath.Dir(dir) {
			if ok, _ := filepath.Match(p, dir); ok {
				return true
			}
			if ok, _ := filepath.Match(p, filepath.Base(dir)); ok {
				return true
			}
		}
	}
	return false
}

// costSet measures the true reclaimable bytes for each candidate.
func costSet(cands []Candidate, opts ScanOptions) {
	if len(cands) == 0 {
		return
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 4
	}
	var (
		wg   sync.WaitGroup
		next atomic.Int64
		done atomic.Int64
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer opts.Priority.ApplyToWorker()()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(cands) {
					return
				}
				// The extent union is a function of the copy set alone,
				// and every member is an immutable snapshot file, so a
				// previous run's answer is still the right one.
				if u, ok := opts.Cache.Usage(cands[i].Copies); ok {
					cands[i].Usage = u
					if n := done.Add(1); n%25 == 0 {
						reportProgress(opts.Progress, "costing", n)
					}
					continue
				}
				paths := make([]string, 0, len(cands[i].Copies))
				for _, c := range cands[i].Copies {
					paths = append(paths, c.Path)
				}
				cands[i].Usage = MeasureSet(paths)
				opts.Cache.PutUsage(cands[i].Copies, cands[i].Usage)
				if n := done.Add(1); n%25 == 0 {
					reportProgress(opts.Progress, "costing", n)
				}
			}
		}()
	}
	wg.Wait()
}

func reportProgress(w io.Writer, label string, n int64) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "\r\033[K  %s: %d...", label, n)
}

func clearProgress(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprint(w, "\r\033[K")
}

// logCandidates records the ranked result so a run can be reviewed afterwards
// without re-running the scan.
func logCandidates(all []Candidate, costed int) {
	if !LogEnabled(LevelInfo) {
		return
	}
	Infof("scan", "ranked %d candidate(s) (%d measured):", len(all), costed)
	var total uint64
	for i, c := range all {
		total += c.Usage.Bytes
		if i >= 100 {
			continue // the log records the ranking, not every long tail row
		}
		Infof("scan", "  #%-4d reclaim=%-12s apparent=%-12s copies=%d/%d method=%-10s exact=%-5v compressed=%-5v kind=%-8s %s:%s",
			c.ID, FormatBytes(c.Usage.Bytes), FormatBytes(c.Apparent),
			len(c.Copies), c.TotalIn, c.Usage.Method, c.Usage.Exact, c.Usage.Compressed,
			c.Kind, c.Pair, c.RelPath)
		for _, cp := range c.Copies {
			Debugf("scan", "        copy snap=%-24s ino=%-10d size=%-12d %s",
				cp.SnapshotID, cp.Ino, cp.Size, cp.Path)
		}
	}
	if len(all) > 100 {
		Infof("scan", "  ... %d further row(s) not itemised", len(all)-100)
	}
	Infof("scan", "total reclaimable across all candidates: %s", FormatBytes(total))
}
