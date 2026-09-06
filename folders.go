package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Kinds of folder rollup.
const (
	FolderDeleted = "deleted" // the directory itself is gone from the live tree
	FolderThinned = "thinned" // the directory is still live; files under it are not
)

// DefaultFolderSample bounds how many file copies one folder's measurement will
// open.
//
// A folder rollup can cover a deleted node_modules of 50,000 files held by
// eight snapshots, and every copy costs an open plus an ioctl or two. Measuring
// 400,000 of them to rank one row would take longer than the whole walk that
// found it, so past this many the folder is measured from a deterministic
// sample and scaled, and the row is marked as an estimate rather than passed
// off as a reading.
const DefaultFolderSample = 20000

// FolderHolder identifies one snapshot holding part of a rollup, with the
// identity the cache uses for immutability: a snapshot whose ctransid still
// matches has not changed since the scan, so the file list purge rebuilds from
// it is the same list the scan measured and displayed.
type FolderHolder struct {
	SnapshotID string `json:"snapshot_id"`
	Root       string `json:"snapshot_root"`
	UUID       string `json:"snapshot_uuid,omitempty"`
	CTransID   uint64 `json:"snapshot_ctransid,omitempty"`
}

// Folder is one directory's rollup: every pinned file underneath it, counted
// once and costed as a set.
//
// It exists because ranking files alone hides the common case. A deleted
// project directory holding 40,000 files of 30 KiB each pins well over a
// gigabyte, and not one of those files comes near a --min-size worth setting
// for a file list. The folder is the thing the user recognises and the thing
// they would delete, so it is the thing the tool should be able to rank.
//
// The member file list is deliberately not stored. A rollup can cover hundreds
// of thousands of files, which would make the scan state larger than the data
// it describes; purge rebuilds the list by walking the folder inside each
// holding snapshot, which is one subtree rather than a whole filesystem.
type Folder struct {
	ID       int    `json:"id"`
	Provider string `json:"provider"`
	Pair     string `json:"pair"`
	Live     string `json:"live"`
	RelPath  string `json:"rel_path"`
	LivePath string `json:"live_path"`
	Kind     string `json:"kind"`

	Files    int            `json:"files"`
	CopyN    int            `json:"copies"`
	Apparent uint64         `json:"apparent_bytes"`
	Usage    SetUsage       `json:"usage"`
	Holders  []FolderHolder `json:"holders"`
	TotalIn  int            `json:"snapshots_in_pair"`

	// Measured is how many copies the usage figure was actually read from.
	// Below CopyN the figure was scaled from a sample.
	Measured int `json:"measured_copies,omitempty"`
}

// Label is the folder's display id. Folder ids share the command line with file
// ids, so they carry a prefix rather than competing for the same numbers.
func (f Folder) Label() string { return fmt.Sprintf("F%d", f.ID) }

// Snaps counts the snapshots holding any part of the folder.
func (f Folder) Snaps() int { return len(f.Holders) }

// folderRollup is the ranked folders plus the member lists their measurement
// needs. Members live only for the duration of a scan.
type folderRollup struct {
	Folders []Folder
	Members [][]*Candidate
}

// liveProbe memoises whether a live path exists. Rolling up a large scan asks
// about the same ancestor directories thousands of times over, and the answer
// cannot change mid-rollup in any way the caller could act on.
type liveProbe struct{ seen map[string]bool }

func newLiveProbe() *liveProbe { return &liveProbe{seen: map[string]bool{}} }

func (p *liveProbe) exists(path string) bool {
	if v, ok := p.seen[path]; ok {
		return v
	}
	var st unix.Stat_t
	v := unix.Lstat(path, &st) == nil
	p.seen[path] = v
	return v
}

// folderKey picks the directory a candidate is reported under.
//
// A deleted tree rolls up to its topmost missing directory rather than to the
// leaf that holds the files. When ~/projects/old-app is gone, the useful row is
// "old-app" once, not one row for each of its forty subdirectories, and the
// figure the user wants is the whole tree's. When every ancestor still exists
// the file was deleted out of a directory that is still there, so the rollup is
// that immediate parent and the folder is reported as thinned rather than gone.
func folderKey(live, rel string, probe *liveProbe) (dir string, deleted bool) {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return ".", false // a file at the subvolume root, which always exists
	}
	parts = parts[:len(parts)-1]
	for i := 1; i <= len(parts); i++ {
		d := strings.Join(parts[:i], "/")
		if !probe.exists(filepath.Join(live, d)) {
			return d, true
		}
	}
	return strings.Join(parts, "/"), false
}

// rollupFolders groups candidates into directories and drops the rollups below
// minSize.
//
// The threshold is applied to apparent size, matching what --min-size means for
// a file list: it selects what is worth measuring, and measurement is what
// produces the reclaim figure the rows are finally ranked on.
func rollupFolders(cands []Candidate, minSize uint64) folderRollup {
	probe := newLiveProbe()

	type group struct {
		folder  Folder
		members []*Candidate
		holders map[string]FolderHolder
	}
	var (
		groups = map[string]*group{}
		order  []string
	)
	for i := range cands {
		c := &cands[i]
		dir, deleted := folderKey(c.Live, c.RelPath, probe)
		key := c.Pair + "\x00" + dir

		g := groups[key]
		if g == nil {
			kind := FolderThinned
			if deleted {
				kind = FolderDeleted
			}
			g = &group{
				folder: Folder{
					Provider: c.Provider,
					Pair:     c.Pair,
					Live:     c.Live,
					RelPath:  dir,
					LivePath: filepath.Join(c.Live, dir),
					Kind:     kind,
					TotalIn:  c.TotalIn,
					// Stays MethodNone unless costFolders measures it, so a
					// rollup beyond --cost-limit renders as unmeasured rather
					// than as zero.
					Usage: SetUsage{Method: MethodNone},
				},
				holders: map[string]FolderHolder{},
			}
			groups[key] = g
			order = append(order, key)
		}

		g.members = append(g.members, c)
		g.folder.Files++
		g.folder.CopyN += len(c.Copies)
		g.folder.Apparent += c.Apparent
		for _, cp := range c.Copies {
			if _, ok := g.holders[cp.SnapshotID]; ok {
				continue
			}
			g.holders[cp.SnapshotID] = FolderHolder{
				SnapshotID: cp.SnapshotID,
				Root:       cp.Snapshot,
				UUID:       cp.SnapshotUUID,
				CTransID:   cp.SnapshotCTransID,
			}
		}
	}

	var out folderRollup
	for _, key := range order {
		g := groups[key]
		if g.folder.Apparent < minSize {
			continue
		}
		for _, h := range g.holders {
			g.folder.Holders = append(g.folder.Holders, h)
		}
		sort.Slice(g.folder.Holders, func(i, j int) bool {
			return g.folder.Holders[i].SnapshotID < g.folder.Holders[j].SnapshotID
		})
		out.Folders = append(out.Folders, g.folder)
		out.Members = append(out.Members, g.members)
	}
	return out
}

// costFolders measures the reclaimable bytes of each rollup.
//
// The union is taken over the whole folder in one pass rather than per file and
// summed, so extents shared between two files in the same tree - a reflinked
// copy, anything a dedupe pass has touched - are counted once, exactly as they
// already are across the snapshots holding one file.
func costFolders(r folderRollup, limit int, opts ScanOptions) {
	if limit <= 0 || limit > len(r.Folders) {
		limit = len(r.Folders)
	}
	if limit == 0 {
		return
	}
	sample := opts.FolderSample
	if sample == 0 {
		sample = DefaultFolderSample
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
				if i >= limit {
					return
				}
				f := &r.Folders[i]
				f.Usage, f.Measured = measureFolder(r.Members[i], sample, opts.Cache)
				if n := done.Add(1); n%5 == 0 {
					reportProgress(opts.Progress, "costing folders", n)
				}
			}
		}()
	}
	wg.Wait()
}

// measureFolder returns a rollup's reclaimable bytes and how many copies that
// figure was read from.
//
// Past the sample budget the folder is measured from an evenly spaced slice of
// its files and scaled by apparent size. Whole files are sampled rather than
// individual copies: the copies of one file are reflinks of each other, and
// splitting them would destroy the very sharing the union exists to account
// for, biasing the estimate upwards by roughly the snapshot count.
func measureFolder(members []*Candidate, sample int, cache *Cache) (SetUsage, int) {
	var (
		copies   int
		apparent uint64
	)
	for _, m := range members {
		copies += len(m.Copies)
		apparent += m.Apparent
	}
	if copies == 0 {
		return SetUsage{Method: MethodNone}, 0
	}

	if sample <= 0 || copies <= sample {
		all := make([]Copy, 0, copies)
		for _, m := range members {
			all = append(all, m.Copies...)
		}
		if u, ok := cache.Usage(all); ok {
			return u, copies
		}
		u := statFallback(MeasureSet(copyPaths(all)), members)
		cache.PutUsage(all, u)
		return u, copies
	}

	// Walk the members with a stride rather than taking a prefix: files in a
	// tree are ordered by path, and a prefix would measure one subdirectory and
	// call it the whole folder.
	stride := copies / sample
	if stride < 1 {
		stride = 1
	}
	var (
		picked        []Copy
		pickedMembers []*Candidate
		pickedBytes   uint64
	)
	for i := 0; i < len(members) && len(picked) < sample; i += stride {
		picked = append(picked, members[i].Copies...)
		pickedMembers = append(pickedMembers, members[i])
		pickedBytes += members[i].Apparent
	}
	// Only the sampled members were read, so they are the ones the fallback
	// has to add up if it is needed.
	u := statFallback(MeasureSet(copyPaths(picked)), pickedMembers)
	if u.Method == MethodNone || pickedBytes == 0 {
		return SetUsage{Method: MethodNone}, 0
	}

	scaled := float64(u.Bytes) * (float64(apparent) / float64(pickedBytes))
	Debugf("measure", "folder sampled: %d/%d file(s), %d/%d copy/copies, %s -> %s scaled",
		len(pickedMembers), len(members), len(picked), copies, FormatBytes(u.Bytes), FormatBytes(uint64(scaled)))
	// Deliberately not cached: the answer depends on the sample budget, so a
	// later run with a larger one must be free to do better than this.
	return SetUsage{
		Bytes:      uint64(scaled),
		Method:     MethodSampled,
		Compressed: u.Compressed,
		Exact:      false,
	}, len(picked)
}

// statFallback substitutes a folder-aware reading whenever MeasureSet had to
// fall back to allocated blocks.
//
// That last resort takes the largest of the paths it was given, which is the
// right answer for the copies of one file - they are reflinks of each other, so
// the set costs what one of them does - and badly wrong for a folder, where
// distinct files occupy distinct extents and would be collapsed to the size of
// the biggest. Only this layer knows which copies belong to which file, so the
// correction belongs here.
func statFallback(u SetUsage, members []*Candidate) SetUsage {
	if u.Method != MethodStat || len(members) == 0 {
		return u
	}
	return statFolder(members)
}

// statFolder sums allocated blocks across distinct files, taking the largest
// copy within each file.
func statFolder(members []*Candidate) SetUsage {
	var total uint64
	for _, m := range members {
		var best uint64
		for _, cp := range m.Copies {
			var st unix.Stat_t
			if unix.Lstat(cp.Path, &st) != nil {
				continue
			}
			if b := uint64(st.Blocks) * 512; b > best {
				best = b
			}
		}
		total += best
	}
	if total == 0 {
		return SetUsage{Method: MethodNone}
	}
	return SetUsage{Bytes: total, Method: MethodStat}
}

func copyPaths(copies []Copy) []string {
	out := make([]string, 0, len(copies))
	for _, c := range copies {
		out = append(out, c.Path)
	}
	return out
}

// rankFolders orders the rollups the way the table shows them and numbers them.
func rankFolders(folders []Folder) {
	sort.Slice(folders, func(i, j int) bool {
		if folders[i].Usage.Bytes != folders[j].Usage.Bytes {
			return folders[i].Usage.Bytes > folders[j].Usage.Bytes
		}
		return folders[i].Apparent > folders[j].Apparent
	})
	for i := range folders {
		folders[i].ID = i + 1
	}
}

// FindFolder returns the rollup with the given display id.
func (st *ScanState) FindFolder(id int) (*Folder, bool) {
	for i := range st.Folders {
		if st.Folders[i].ID == id {
			return &st.Folders[i], true
		}
	}
	return nil, false
}

// ExpandFolder rebuilds the member files of a rollup by walking it inside each
// snapshot that holds it, and returns them as candidates for the ordinary purge
// plan.
//
// The list is rebuilt rather than replayed from the scan because storing it
// would mean writing a record per file for folders that hold hundreds of
// thousands. What makes that safe is the check below: a snapshot is read-only
// and its ctransid moves on any change, so a holder whose ctransid still
// matches the scan cannot have gained or lost a file since. That is the same
// invariant the manifest cache relies on, and it is a stronger statement than
// the per-file mtime comparison it stands in for. Every other guard is
// unchanged, because these candidates go through BuildPlan like any other.
func ExpandFolder(f *Folder, pairs []Pair, nextID int) ([]Candidate, []Skip, error) {
	snaps := map[string]Snapshot{}
	var live, provider string
	found := false
	for _, p := range pairs {
		if p.Name != f.Pair {
			continue
		}
		found = true
		live, provider = p.Live, p.Provider
		for _, s := range p.Snapshots {
			snaps[s.ID] = s
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("folder %s: pair %q is not in the current scan", f.Label(), f.Pair)
	}

	// A directory that has come back is a different decision than the one the
	// user reviewed, exactly as a returning file is in BuildPlan.
	livePath := filepath.Join(live, f.RelPath)
	if f.Kind == FolderDeleted {
		if _, err := os.Lstat(livePath); err == nil {
			return nil, []Skip{{f.RelPath, "directory exists on the live filesystem again"}}, nil
		}
	}

	var (
		skips   []Skip
		byPath  = map[string][]Copy{}
		holders int
	)
	for _, h := range f.Holders {
		snap, ok := snaps[h.SnapshotID]
		if !ok {
			skips = append(skips, Skip{h.SnapshotID, "snapshot is no longer present (rotated out)"})
			continue
		}
		// The one check that replaces re-reading every file's mtime. Without
		// it the rebuilt list could differ from the one that was measured and
		// shown, and the user would be approving a different thing.
		if h.CTransID != 0 && snap.CTransID != 0 && snap.CTransID != h.CTransID {
			skips = append(skips, Skip{snap.ID,
				fmt.Sprintf("snapshot changed since the scan (ctransid %d -> %d); rescan before purging this folder",
					h.CTransID, snap.CTransID)})
			continue
		}
		root := filepath.Join(snap.Root, f.RelPath)
		if _, err := os.Lstat(root); err != nil {
			skips = append(skips, Skip{root, "folder is not in this snapshot any more"})
			continue
		}
		entries, complete, err := readdirWalk(root, 0, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("folder %s in %s: %w", f.Label(), snap.ID, err)
		}
		if !complete {
			return nil, nil, fmt.Errorf("folder %s in %s: could not read the whole subtree; "+
				"purging part of it would free nothing", f.Label(), snap.ID)
		}
		holders++
		for _, e := range entries {
			rel := filepath.Join(f.RelPath, e.Rel)
			byPath[rel] = append(byPath[rel], Copy{
				SnapshotID:       snap.ID,
				Snapshot:         snap.Root,
				Path:             filepath.Join(snap.Root, rel),
				Ino:              e.Ino,
				Size:             e.Size,
				MtimeNs:          e.MtimeNs,
				SnapshotUUID:     snap.UUID,
				SnapshotCTransID: snap.CTransID,
			})
		}
	}
	if holders == 0 {
		return nil, skips, nil
	}

	rels := make([]string, 0, len(byPath))
	for rel := range byPath {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	// The folder's measured reclaim is a property of the whole set - the
	// extents are shared, so there is no exact per-file figure to hand out.
	// Splitting it by apparent size keeps the plan's total equal to the number
	// the user was shown, which is the figure that has to be right.
	var totalApparent uint64
	for _, rel := range rels {
		var biggest uint64
		for _, cp := range byPath[rel] {
			if cp.Size > biggest {
				biggest = cp.Size
			}
		}
		totalApparent += biggest
	}

	out := make([]Candidate, 0, len(rels))
	for _, rel := range rels {
		copies := byPath[rel]
		// A file that is back on the live tree is no longer pinned by these
		// snapshots alone, and was not part of what the folder promised.
		if _, err := os.Lstat(filepath.Join(live, rel)); err == nil {
			skips = append(skips, Skip{rel, "file exists on the live filesystem again"})
			continue
		}
		var apparent uint64
		for _, cp := range copies {
			if cp.Size > apparent {
				apparent = cp.Size
			}
		}
		share := SetUsage{Method: MethodNone}
		if f.Usage.Method != MethodNone && totalApparent > 0 {
			share = SetUsage{
				Bytes:      uint64(float64(f.Usage.Bytes) * (float64(apparent) / float64(totalApparent))),
				Method:     f.Usage.Method,
				Compressed: f.Usage.Compressed,
				Exact:      false, // a share of a set figure, never a reading
			}
		}
		sort.Slice(copies, func(i, j int) bool { return copies[i].SnapshotID < copies[j].SnapshotID })
		out = append(out, Candidate{
			ID:       nextID,
			Provider: provider,
			Pair:     f.Pair,
			Live:     live,
			RelPath:  rel,
			LivePath: filepath.Join(live, rel),
			Kind:     KindDeleted,
			Apparent: apparent,
			Copies:   copies,
			Usage:    share,
			TotalIn:  f.TotalIn,
		})
		nextID++
	}
	Infof("plan", "folder %s (%s): rebuilt %d file(s) across %d snapshot(s), %d skip(s)",
		f.Label(), f.RelPath, len(out), holders, len(skips))
	return out, skips, nil
}
