package main

import (
	"os"
	"path/filepath"
	"sort"
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

func TestDefaultWorkersIsBounded(t *testing.T) {
	n := DefaultWorkers()
	if n < 2 || n > 8 {
		t.Errorf("DefaultWorkers() = %d, want between 2 and 8", n)
	}
}
