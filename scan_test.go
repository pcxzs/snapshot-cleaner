package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// relPaths reduces a walk result to the paths it found, which is what the walk
// tests assert on.
func relPaths(entries []manifestEntry, complete bool, err error) ([]string, bool, error) {
	var out []string
	for _, e := range entries {
		out = append(out, e.Rel)
	}
	return out, complete, err
}

func counterValue(name string) int64 {
	logbook.mu.Lock()
	defer logbook.mu.Unlock()
	if c, ok := logbook.counters[name]; ok {
		return c.Load()
	}
	return 0
}

func lstat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestReaddirWalkAppliesSizeThreshold(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel string, size int) {
		if err := os.WriteFile(filepath.Join(root, rel), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("big.bin", 4096)
	write("small.bin", 10)
	write("sub/also-big.bin", 8192)

	got, complete, err := relPaths(readdirWalk(root, 1024, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Error("a fully readable tree must report a complete walk")
	}
	sort.Strings(got)
	want := []string{"big.bin", "sub/also-big.bin"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReaddirWalkSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink to a large file must not be reported: it holds no extents, and
	// following it would take the walk outside the snapshot.
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}

	got, _, err := relPaths(readdirWalk(root, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "real.bin" {
		t.Fatalf("got %v, want only real.bin", got)
	}
}

func TestReaddirWalkHonoursExcludes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"keep.iso", "drop.log", "cache/junk.bin"} {
		if err := os.WriteFile(filepath.Join(root, rel), make([]byte, 2048), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := relPaths(readdirWalk(root, 1, []string{"*.log", "cache"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "keep.iso" {
		t.Fatalf("got %v, want only keep.iso", got)
	}
}

// A snapshot copy identical to the live file shares every extent with it and
// frees nothing, so it must not become a candidate.
func TestFilterDifferingDropsIdenticalCopies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := lstat(t, path)

	from := func(st unix.Stat_t) found {
		return found{entry: manifestEntry{Ino: st.Ino, Size: uint64(st.Size), MtimeNs: st.Mtim.Nano()}}
	}
	same := from(live)
	differentIno := from(live)
	differentIno.entry.Ino++
	differentSize := from(live)
	differentSize.entry.Size++
	differentMtime := from(live)
	differentMtime.entry.MtimeNs ^= 1

	got := filterDiffering([]found{same, differentIno, differentSize, differentMtime}, &live)
	if len(got) != 3 {
		t.Fatalf("got %d copies, want 3 (the identical one must be dropped)", len(got))
	}
}

func TestExcludedMatchesSubtrees(t *testing.T) {
	// filepath.Match's * never crosses a /, so a pattern only reaches a nested
	// file through the ancestor walk. Without it, --exclude silently kept
	// everything below the first level.
	cases := []struct {
		name    string
		pattern string
		rel     string
		want    bool
	}{
		{"glob one level down", "deep/*", "deep/buried.bin", true},
		{"glob two levels down", "deep/*", "deep/nested/buried.bin", true},
		{"glob many levels down", "deep/*", "deep/a/b/c/buried.bin", true},
		{"bare directory name", "deep", "deep/nested/buried.bin", true},
		{"intermediate directory", "nested", "deep/nested/buried.bin", true},
		{"full path", "deep/nested/buried.bin", "deep/nested/buried.bin", true},
		{"basename glob", "*.iso", "downloads/junk.iso", true},
		{"basename glob at root", "*.iso", "junk.iso", true},
		{"wildcard directory", "*/nested", "deep/nested/buried.bin", true},
		{"unrelated directory", "shallow", "deep/nested/buried.bin", false},
		{"unrelated glob", "*.qcow2", "deep/nested/buried.bin", false},
		{"partial name is not a match", "dee", "deep/nested/buried.bin", false},
		{"sibling of the target", "deep/other/*", "deep/nested/buried.bin", false},
		{"no patterns", "", "deep/nested/buried.bin", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var patterns []string
			if tc.pattern != "" {
				patterns = []string{tc.pattern}
			}
			got := excluded(tc.rel, filepath.Base(tc.rel), patterns)
			if got != tc.want {
				t.Errorf("excluded(%q, pattern %q) = %v, want %v",
					tc.rel, tc.pattern, got, tc.want)
			}
		})
	}
}

func TestExcludedMatchesAnyPattern(t *testing.T) {
	patterns := []string{"*.tmp", "deep/*"}
	if !excluded("deep/nested/buried.bin", "buried.bin", patterns) {
		t.Error("second pattern must still be consulted")
	}
	if !excluded("scratch.tmp", "scratch.tmp", patterns) {
		t.Error("first pattern must still be consulted")
	}
	if excluded("keep/me.iso", "me.iso", patterns) {
		t.Error("a path matching neither pattern must survive")
	}
}

func TestSelectScope(t *testing.T) {
	pairs := []Pair{
		{Name: "@home", Live: "/home"},
		{Name: "@", Live: "/"},
	}
	if got, err := selectScope(pairs, "all"); err != nil || len(got) != 2 {
		t.Fatalf("all: got %d, err %v", len(got), err)
	}
	if got, err := selectScope(pairs, ""); err != nil || len(got) != 2 {
		t.Fatalf("empty means all: got %d, err %v", len(got), err)
	}
	got, err := selectScope(pairs, "@home")
	if err != nil || len(got) != 1 || got[0].Name != "@home" {
		t.Fatalf("by name: got %v, err %v", got, err)
	}
	if got, err := selectScope(pairs, "/home"); err != nil || len(got) != 1 {
		t.Fatalf("by live path: got %d, err %v", len(got), err)
	}
	if _, err := selectScope(pairs, "nope"); err == nil {
		t.Error("an unmatched scope must be an error, not an empty scan")
	}
}

func TestDedupePairs(t *testing.T) {
	a := Pair{Provider: "timeshift", Name: "@home", Live: "/home",
		Snapshots: []Snapshot{{Root: "/snap/1"}}}
	b := Pair{Provider: "snapper", Name: "home", Live: "/home",
		Snapshots: []Snapshot{{Root: "/snap/1"}}} // same live path, same snapshots
	c := Pair{Provider: "timeshift", Name: "@", Live: "/",
		Snapshots: []Snapshot{{Root: "/snap/2"}}}

	got := dedupePairs([]Pair{a, b, c})
	if len(got) != 2 {
		t.Fatalf("got %d pairs, want 2 (the duplicate layout must collapse)", len(got))
	}
}

func TestSortSnapshotsNewestFirst(t *testing.T) {
	s := []Snapshot{
		{ID: "old", Created: mustTime("2026-01-01 00:00:00")},
		{ID: "new", Created: mustTime("2026-09-01 00:00:00")},
		{ID: "mid", Created: mustTime("2026-05-01 00:00:00")},
	}
	sortSnapshots(s)
	if s[0].ID != "new" || s[2].ID != "old" {
		t.Fatalf("bad order: %s %s %s", s[0].ID, s[1].ID, s[2].ID)
	}
}

func mustTime(s string) time.Time {
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		panic(err)
	}
	return v
}

// A snapshot whose root cannot be read must produce an error rather than
// wedging the worker pool. With more snapshots than workers, a worker that
// returned early would leave the sender blocked forever.
func TestScanPairSurvivesUnreadableSnapshot(t *testing.T) {
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "big.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	pair := Pair{Name: "test", Live: t.TempDir()}
	for i := 0; i < 12; i++ {
		pair.Snapshots = append(pair.Snapshots, Snapshot{
			ID:   filepath.Base(good) + string(rune('a'+i)),
			Root: filepath.Join(good, "does-not-exist"),
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := scanPair(pair, ScanOptions{MinSize: 1, Workers: 2}); err == nil {
			t.Error("expected an error for unreadable snapshot roots")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scanPair deadlocked on unreadable snapshots")
	}
}

// The live index has to reach the same conclusions the end-of-pair diff does,
// because its whole job is to reach them earlier - before the record it is
// deciding has been retained.
func TestLiveIndexAgreesWithTheDiff(t *testing.T) {
	live := manifestEntry{Rel: "a/f", Ino: 7, Size: 100, MtimeNs: 42}
	x := liveIndex{files: map[string]manifestEntry{"a/f": live}, ok: true}

	differs := live
	differs.MtimeNs++
	gone := manifestEntry{Rel: "a/deleted", Ino: 9, Size: 100}

	cases := []struct {
		name            string
		entry           manifestEntry
		includeReplaced bool
		want            int
	}{
		{"identical to live", live, false, dropIdentical},
		{"identical to live, --include-replaced", live, true, dropIdentical},
		{"still live but changed", differs, false, dropReplaced},
		{"still live but changed, --include-replaced", differs, true, keepEntry},
		{"not live at all", gone, false, keepEntry},
	}
	for _, tc := range cases {
		if got := x.verdict(tc.entry, tc.includeReplaced); got != tc.want {
			t.Errorf("%s: verdict = %d, want %d", tc.name, got, tc.want)
		}
	}

	// An index that could not be built decides nothing, leaving every entry to
	// the diff exactly as before it existed.
	var none liveIndex
	if got := none.verdict(live, false); got != keepEntry {
		t.Errorf("verdict without an index = %d, want %d", got, keepEntry)
	}
}

// A snapshot copy identical to the live file must be dropped as it is walked,
// not held until the diff at the end of the pair. That is the whole memory
// argument for the live index, so it is asserted directly rather than inferred
// from the candidates, which look the same either way.
func TestScanPairDropsCopiesIdenticalToLiveAsItWalks(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}

	// Hardlinks stand in for reflinks here: same inode, size and mtime, which
	// is what a snapshot copy of an untouched file looks like.
	const (
		untouched = 10
		snapshots = 3
	)
	for i := 0; i < untouched; i++ {
		name := fmt.Sprintf("untouched%d.bin", i)
		if err := os.WriteFile(filepath.Join(live, name), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var snaps []Snapshot
	for s := 0; s < snapshots; s++ {
		root := filepath.Join(base, fmt.Sprintf("snap%d", s))
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < untouched; i++ {
			name := fmt.Sprintf("untouched%d.bin", i)
			if err := os.Link(filepath.Join(live, name), filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "deleted.bin"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, Snapshot{ID: fmt.Sprintf("s%d", s), Root: root})
	}

	pair := Pair{Name: "test", Live: live, Snapshots: snaps}
	before := counterValue("walk.identical_to_live")
	got, err := scanPair(pair, ScanOptions{MinSize: 1, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if dropped := counterValue("walk.identical_to_live") - before; dropped != untouched*snapshots {
		t.Errorf("dropped %d copies during the walk, want all %d of them",
			dropped, untouched*snapshots)
	}
	if len(got) != 1 || got[0].RelPath != "deleted.bin" || len(got[0].Copies) != snapshots {
		t.Fatalf("got %+v, want the one deleted file, held by every snapshot", got)
	}
}

// The index is not free - it is a whole tree sweep, and the live tree is the
// one part of a scan that can never be cached - so an ordinary file scan, which
// retains a few thousand records and diffs them cheaply, must not pay for it.
func TestLiveIndexIsBuiltOnlyForLowThresholdScans(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "live")
	snap := filepath.Join(base, "snap")
	for _, d := range []string{live, snap} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(snap, "gone.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	pair := Pair{Name: "test", Live: live, Snapshots: []Snapshot{{ID: "s1", Root: snap}}}

	built := func(opts ScanOptions) int64 {
		before := counterValue("walk.live_index_built")
		if _, err := scanPair(pair, opts); err != nil {
			t.Fatal(err)
		}
		return counterValue("walk.live_index_built") - before
	}
	if n := built(ScanOptions{MinSize: 50 << 20, Workers: 1}); n != 0 {
		t.Errorf("a 50 MiB file scan built the live index %d time(s), want 0", n)
	}
	if n := built(ScanOptions{MinSize: 0, Workers: 1, Folders: true}); n != 1 {
		t.Errorf("a folder scan built the live index %d time(s), want 1", n)
	}
}

// The other half of that: what is still live but changed is only retained when
// the scan was asked for it.
func TestScanPairRetainsReplacedFilesOnlyWhenAsked(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "live")
	snap := filepath.Join(base, "snap")
	for _, d := range []string{live, snap} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, "edited.bin"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "edited.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	pair := Pair{Name: "test", Live: live, Snapshots: []Snapshot{{ID: "s1", Root: snap}}}

	got, err := scanPair(pair, ScanOptions{MinSize: 1, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing without --include-replaced", got)
	}

	got, err = scanPair(pair, ScanOptions{MinSize: 1, Workers: 1, IncludeReplaced: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != KindReplaced {
		t.Fatalf("got %+v, want the replaced file with --include-replaced", got)
	}
}

// Running out of memory tells the user nothing about which flag to reach for,
// so a scan that cannot fit says so while it still can.
func TestScanPairStopsAtTheEntryBudget(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "live")
	snap := filepath.Join(base, "snap")
	for _, d := range []string{live, snap} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		if err := os.WriteFile(filepath.Join(snap, fmt.Sprintf("gone%d.bin", i)), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pair := Pair{Name: "@home", Live: live, Snapshots: []Snapshot{{ID: "s1", Root: snap}}}

	_, err := scanPair(pair, ScanOptions{MinSize: 1, Workers: 1, MaxEntries: 3, Folders: true})
	if err == nil {
		t.Fatal("expected the budget to stop the scan")
	}
	for _, want := range []string{"@home", "--file-min-size", "--max-entries"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	// The live index is held for the whole pair, so it counts against the same
	// ceiling rather than being spent before the ceiling is consulted.
	for i := 0; i < 8; i++ {
		if err := os.WriteFile(filepath.Join(live, fmt.Sprintf("here%d.bin", i)), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	empty := Pair{Name: "@home", Live: live, Snapshots: []Snapshot{{ID: "s1", Root: filepath.Join(base, "none")}}}
	if _, err := scanPair(empty, ScanOptions{MinSize: 1, Workers: 1, MaxEntries: 3}); err == nil {
		t.Error("expected a live tree past the budget to stop the scan")
	}

	// A negative budget is the way out for a machine with the memory to spare.
	if _, err := scanPair(pair, ScanOptions{MinSize: 1, Workers: 1, MaxEntries: -1}); err != nil {
		t.Errorf("a negative budget should lift the ceiling, got %v", err)
	}
}

func TestDefaultWorkersIsBounded(t *testing.T) {
	n := DefaultWorkers()
	if n < 2 || n > 8 {
		t.Errorf("DefaultWorkers() = %d, want between 2 and 8", n)
	}
}
