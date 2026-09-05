package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The integration test builds a real btrfs filesystem in a loopback image and
// exercises the destructive path there. It never touches the host's own
// snapshots: everything happens inside a throwaway image that is destroyed
// afterwards.
//
//	sudo SNAPSHOT_CLEANER_INTEGRATION=1 go test -run Integration -v
//
// TestMain turns on full logging for integration runs, so the destructive
// path leaves a reviewable record of exactly what it did.
func TestMain(m *testing.M) {
	if path := os.Getenv("SNAPSHOT_CLEANER_LOG"); path != "" {
		InitLog(filepath.Dir(path), LevelTrace, nil, path)
		LogEnvironment(os.Args)
		fmt.Fprintf(os.Stderr, "integration log: %s\n", path)
	}
	code := m.Run()
	LogCounters()
	CloseLog()
	os.Exit(code)
}

func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SNAPSHOT_CLEANER_INTEGRATION") != "1" {
		t.Skip("set SNAPSHOT_CLEANER_INTEGRATION=1 to run (needs root and a loop device)")
	}
	if os.Geteuid() != 0 {
		t.Fatal("integration tests need root")
	}
	for _, tool := range []string{"mkfs.btrfs", "losetup", "mount", "umount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

// testFS is a scratch btrfs filesystem on a loop device.
type testFS struct {
	t     *testing.T
	image string
	loop  string
	mount string
}

func newTestFS(t *testing.T, sizeMB int, mountOpts string) *testFS {
	t.Helper()
	dir := t.TempDir()
	fs := &testFS{t: t, image: filepath.Join(dir, "btrfs.img"), mount: filepath.Join(dir, "mnt")}

	if err := os.Truncate(fs.image, 0); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	f, err := os.Create(fs.image)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	mustRun(t, "mkfs.btrfs", "-q", "-f", fs.image)

	out, err := exec.Command("losetup", "--find", "--show", fs.image).Output()
	if err != nil {
		t.Fatalf("losetup: %v", err)
	}
	fs.loop = strings.TrimSpace(string(out))

	if err := os.MkdirAll(fs.mount, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "mount", "-o", mountOpts, fs.loop, fs.mount)
	t.Cleanup(fs.close)
	return fs
}

func (fs *testFS) close() {
	exec.Command("umount", fs.mount).Run()
	if fs.loop != "" {
		exec.Command("losetup", "-d", fs.loop).Run()
	}
}

func (fs *testFS) path(parts ...string) string {
	return filepath.Join(append([]string{fs.mount}, parts...)...)
}

// snapshot makes a read-only snapshot the way a snapshot manager would.
func (fs *testFS) snapshot(src, dst string) {
	fs.t.Helper()
	mustRun(fs.t, "btrfs", "subvolume", "snapshot", "-r", src, dst)
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// writeIncompressible writes data zstd cannot shrink, so the apparent size and
// the on-disk size agree and the arithmetic is easy to assert on.
func writeIncompressible(t *testing.T, path string, size int) {
	t.Helper()
	data := make([]byte, size)
	// A simple LCG: cheap, deterministic, and not compressible.
	x := uint32(12345)
	for i := range data {
		x = x*1103515245 + 12345
		data[i] = byte(x >> 16)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	syncPath(t, path)
}

func syncPath(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	unix.Sync()
}

func TestIntegrationMeasureAndPurge(t *testing.T) {
	requireIntegration(t)

	fs := newTestFS(t, 1024, "compress=zstd:1")
	live := fs.path("live")
	snaps := fs.path("snapshots")
	mustRun(t, "btrfs", "subvolume", "create", live)
	if err := os.MkdirAll(snaps, 0o755); err != nil {
		t.Fatal(err)
	}

	const fileSize = 64 << 20
	junk := filepath.Join(live, "junk.iso")
	keep := filepath.Join(live, "keep.iso")
	writeIncompressible(t, junk, fileSize)
	writeIncompressible(t, keep, fileSize)

	// Three snapshots all holding junk.iso, exactly the situation the tool is
	// for: one file reflinked into several snapshots.
	for _, name := range []string{"snap1", "snap2", "snap3"} {
		fs.snapshot(live, filepath.Join(snaps, name))
	}

	// Delete it from the live tree. Space is not freed: the snapshots pin it.
	if err := os.Remove(junk); err != nil {
		t.Fatal(err)
	}
	unix.Sync()

	pair, err := DetectGeneric(live, snaps)
	if err != nil {
		t.Fatalf("DetectGeneric: %v", err)
	}
	if len(pair.Snapshots) != 3 {
		t.Fatalf("found %d snapshots, want 3", len(pair.Snapshots))
	}
	for _, s := range pair.Snapshots {
		if !s.ReadOnly {
			t.Fatalf("snapshot %s should have been detected as read-only", s.ID)
		}
	}

	cands, err := Scan([]Pair{pair}, ScanOptions{MinSize: 1 << 20, CostLimit: 0})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want exactly 1 (junk.iso); got %+v", len(cands), cands)
	}

	c := cands[0]
	if c.RelPath != "junk.iso" {
		t.Fatalf("wrong candidate: %s", c.RelPath)
	}
	if c.Kind != KindDeleted {
		t.Fatalf("kind = %s, want %s", c.Kind, KindDeleted)
	}
	if len(c.Copies) != 3 {
		t.Fatalf("found %d copies, want 3", len(c.Copies))
	}
	if c.Usage.Method != MethodTreeSearch {
		t.Errorf("measured via %s; expected the exact TREE_SEARCH_V2 path as root", c.Usage.Method)
	}

	// The reclaim figure is the size of the file's extents, counted once - NOT
	// three times, even though three snapshots reference them.
	if c.Usage.Bytes < fileSize*95/100 || c.Usage.Bytes > fileSize*110/100 {
		t.Fatalf("reclaim = %s, want about %s (shared extents must count once, not 3x)",
			FormatBytes(c.Usage.Bytes), FormatBytes(fileSize))
	}
	t.Logf("candidate: %s reclaim=%s apparent=%s copies=%d method=%s",
		c.RelPath, FormatBytes(c.Usage.Bytes), FormatBytes(c.Apparent), len(c.Copies), c.Usage.Method)

	// keep.iso is identical in every snapshot and on the live tree, so it must
	// not be reported: removing it from snapshots would free nothing.
	for _, cand := range cands {
		if cand.RelPath == "keep.iso" {
			t.Error("keep.iso is still live and identical; it must not be a candidate")
		}
	}

	st := &ScanState{Pairs: []Pair{pair}, Candidates: cands, Costed: len(cands)}
	plan, err := BuildPlan(st, []int{c.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 3 {
		t.Fatalf("plan has %d targets, want 3 (all holders); skips: %+v", len(plan.Targets), plan.Skips)
	}

	freeBefore := freeBytes(t, fs.mount)
	purger := &Purger{Journal: nil, Out: &testWriter{t}, Mounter: NewMounter(t.TempDir())}
	n, err := purger.Execute(plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n != 3 {
		t.Fatalf("removed %d copies, want 3", n)
	}
	unix.Sync()
	freeAfter := freeBytes(t, fs.mount)

	// Every snapshot must be back to read-only and still be a valid subvolume.
	for _, s := range pair.Snapshots {
		ro, err := IsReadOnlySubvol(s.Root)
		if err != nil {
			t.Fatalf("snapshot %s is no longer a readable subvolume: %v", s.ID, err)
		}
		if !ro {
			t.Fatalf("snapshot %s was left writable", s.ID)
		}
		// The snapshot must still be intact - the other file is untouched.
		if _, err := os.Stat(filepath.Join(s.Root, "keep.iso")); err != nil {
			t.Fatalf("snapshot %s lost keep.iso: %v", s.ID, err)
		}
		if _, err := os.Stat(filepath.Join(s.Root, "junk.iso")); !os.IsNotExist(err) {
			t.Fatalf("snapshot %s still holds junk.iso", s.ID)
		}
	}

	freed := int64(freeAfter) - int64(freeBefore)
	t.Logf("free: %s -> %s (freed %s, predicted %s)",
		FormatBytes(freeBefore), FormatBytes(freeAfter), FormatBytes(uint64(max64(freed, 0))), FormatBytes(c.Usage.Bytes))
	if freed < int64(fileSize)*80/100 {
		t.Fatalf("only %d bytes were freed; predicted %d", freed, c.Usage.Bytes)
	}
}

// A file removed from only some of the snapshots holding it frees nothing.
// This is the property the "all holders or none" rule exists to protect.
func TestIntegrationPartialPurgeFreesNothing(t *testing.T) {
	requireIntegration(t)

	fs := newTestFS(t, 1024, "compress=zstd:1")
	live := fs.path("live")
	snaps := fs.path("snapshots")
	mustRun(t, "btrfs", "subvolume", "create", live)
	if err := os.MkdirAll(snaps, 0o755); err != nil {
		t.Fatal(err)
	}

	const fileSize = 32 << 20
	writeIncompressible(t, filepath.Join(live, "junk.iso"), fileSize)
	for _, name := range []string{"snap1", "snap2"} {
		fs.snapshot(live, filepath.Join(snaps, name))
	}
	if err := os.Remove(filepath.Join(live, "junk.iso")); err != nil {
		t.Fatal(err)
	}
	unix.Sync()

	pair, err := DetectGeneric(live, snaps)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := Scan([]Pair{pair}, ScanOptions{MinSize: 1 << 20, CostLimit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || len(cands[0].Copies) != 2 {
		t.Fatalf("unexpected scan result: %+v", cands)
	}

	// Purge only the first holder.
	st := &ScanState{Pairs: []Pair{pair}, Candidates: cands}
	plan, err := BuildPlan(st, []int{cands[0].ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	plan.Targets = plan.Targets[:1]

	freeBefore := freeBytes(t, fs.mount)
	purger := &Purger{Out: &testWriter{t}, Mounter: NewMounter(t.TempDir())}
	if _, err := purger.Execute(plan); err != nil {
		t.Fatal(err)
	}
	unix.Sync()
	freed := int64(freeBytes(t, fs.mount)) - int64(freeBefore)

	t.Logf("partial purge freed %d bytes of a %s file", freed, FormatBytes(fileSize))
	if freed > int64(fileSize)/2 {
		t.Fatalf("partial purge freed %d bytes; the extents are still pinned by the other snapshot, so it should free ~nothing", freed)
	}
}

func TestIntegrationTreeSearchMatchesCompsize(t *testing.T) {
	requireIntegration(t)
	if _, err := exec.LookPath("compsize"); err != nil {
		t.Skip("compsize not installed; nothing to cross-check against")
	}

	fs := newTestFS(t, 1024, "compress=zstd:1")
	live := fs.path("live")
	mustRun(t, "btrfs", "subvolume", "create", live)

	// Compressible data, which is where FIEMAP overstates and TREE_SEARCH_V2
	// should agree with compsize.
	path := filepath.Join(live, "text.dat")
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 400000))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	syncPath(t, path)

	got := MeasureSet([]string{path})
	if got.Method != MethodTreeSearch {
		t.Fatalf("method = %s, want %s", got.Method, MethodTreeSearch)
	}
	if !got.Compressed {
		t.Error("expected the extents to be reported as compressed")
	}

	want := compsizeDiskUsage(t, path)
	diff := int64(got.Bytes) - int64(want)
	if diff < 0 {
		diff = -diff
	}
	t.Logf("apparent=%s  ours=%s  compsize=%s", FormatBytes(uint64(len(data))), FormatBytes(got.Bytes), FormatBytes(want))
	// Allow a little slack for metadata accounting differences.
	if want > 0 && diff*100/int64(want) > 5 {
		t.Fatalf("our figure %d differs from compsize %d by more than 5%%", got.Bytes, want)
	}
	if got.Bytes >= uint64(len(data)) {
		t.Errorf("compressed file measured at %d, not below its apparent size %d", got.Bytes, len(data))
	}
}

// compsizeDiskUsage parses the "Disk Usage" column of compsize's TOTAL row.
func compsizeDiskUsage(t *testing.T, path string) uint64 {
	t.Helper()
	out, err := exec.Command("compsize", "-b", path).Output()
	if err != nil {
		t.Fatalf("compsize: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "TOTAL" {
			var v uint64
			if _, err := fmt.Sscanf(fields[2], "%d", &v); err == nil {
				return v
			}
		}
	}
	t.Fatalf("could not parse compsize output:\n%s", out)
	return 0
}

func freeBytes(t *testing.T, path string) uint64 {
	t.Helper()
	n, err := FreeBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// buildCacheTestFS makes a filesystem with one live subvolume, three snapshots
// holding a file that is then deleted from the live tree - the situation every
// cache test below varies from.
func buildCacheTestFS(t *testing.T) (fs *testFS, pair Pair) {
	t.Helper()
	fs = newTestFS(t, 1024, "compress=zstd:1")
	live := fs.path("live")
	snaps := fs.path("snapshots")
	mustRun(t, "btrfs", "subvolume", "create", live)
	if err := os.MkdirAll(snaps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(live, "deep", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIncompressible(t, filepath.Join(live, "junk.iso"), 32<<20)
	writeIncompressible(t, filepath.Join(live, "deep", "nested", "buried.bin"), 24<<20)
	writeIncompressible(t, filepath.Join(live, "keep.iso"), 16<<20)

	for _, name := range []string{"snap1", "snap2", "snap3"} {
		fs.snapshot(live, filepath.Join(snaps, name))
	}
	if err := os.Remove(filepath.Join(live, "junk.iso")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "deep", "nested", "buried.bin")); err != nil {
		t.Fatal(err)
	}
	unix.Sync()

	var err error
	if pair, err = DetectGeneric(live, snaps); err != nil {
		t.Fatalf("DetectGeneric: %v", err)
	}
	for _, s := range pair.Snapshots {
		if !s.Cacheable() {
			t.Fatalf("snapshot %s has no cache identity (uuid=%q ctransid=%d)", s.ID, s.UUID, s.CTransID)
		}
	}
	return fs, pair
}

// summarise reduces a scan result to something comparable across runs. IDs are
// positions in a ranking and say nothing about whether two scans agree.
func summarise(cands []Candidate) []string {
	var out []string
	for _, c := range cands {
		out = append(out, fmt.Sprintf("%s|%s|%d|%d|%d", c.RelPath, c.Kind, c.Apparent, c.Usage.Bytes, len(c.Copies)))
	}
	sort.Strings(out)
	return out
}

func scanWith(t *testing.T, pair Pair, opts ScanOptions) []Candidate {
	t.Helper()
	if opts.MinSize == 0 {
		opts.MinSize = 1 << 20
	}
	cands, err := Scan([]Pair{pair}, opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return cands
}

// The test that matters most: a cached scan and a cold one must agree. If they
// ever disagree, the cache is reporting something that is not on the disk.
func TestIntegrationCachedScanMatchesColdScan(t *testing.T) {
	requireIntegration(t)
	fs, pair := buildCacheTestFS(t)
	_ = fs

	dirs := Dirs{State: t.TempDir(), Runtime: t.TempDir()}

	cold := OpenCache(dirs, CacheOptions{})
	want := summarise(scanWith(t, pair, ScanOptions{Cache: cold, CacheFloor: 1 << 20}))
	if cold.Stats.ManifestHits.Load() != 0 {
		t.Errorf("the first scan cannot have had anything to reuse (%d hits)", cold.Stats.ManifestHits.Load())
	}
	cold.Close()

	warm := OpenCache(dirs, CacheOptions{})
	got := summarise(scanWith(t, pair, ScanOptions{Cache: warm, CacheFloor: 1 << 20}))
	hits := warm.Stats.ManifestHits.Load()
	warm.Close()

	if hits != int64(len(pair.Snapshots)) {
		t.Errorf("reused %d of %d snapshot listings; the cache is not being used", hits, len(pair.Snapshots))
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("a cached scan disagreed with a cold one:\n cold: %v\n warm: %v", want, got)
	}

	// --refresh must rewalk and still reach the same answer.
	fresh := OpenCache(dirs, CacheOptions{Refresh: true})
	again := summarise(scanWith(t, pair, ScanOptions{Cache: fresh, CacheFloor: 1 << 20}))
	if fresh.Stats.ManifestHits.Load() != 0 {
		t.Error("--refresh must not reuse anything")
	}
	fresh.Close()
	if !reflect.DeepEqual(want, again) {
		t.Fatalf("--refresh disagreed with the cold scan:\n cold: %v\n fresh: %v", want, again)
	}
}

// The two walks read the same filesystem through completely different kernel
// interfaces. They must produce the same list, or the offsets in treewalk.go
// are wrong.
func TestIntegrationTreeWalkMatchesReaddirWalk(t *testing.T) {
	requireIntegration(t)
	fs, pair := buildCacheTestFS(t)
	_ = fs

	for _, snap := range pair.Snapshots {
		if err := TreeWalkSupported(snap.Root); err != nil {
			t.Skipf("tree search unavailable: %v", err)
		}
		tree, err := treeWalkSnapshot(snap.Root, 1<<20)
		if err != nil {
			t.Fatalf("tree walk of %s: %v", snap.ID, err)
		}
		readdir, complete, err := readdirWalk(snap.Root, 1<<20, nil)
		if err != nil {
			t.Fatalf("readdir walk of %s: %v", snap.ID, err)
		}
		if !complete {
			t.Fatalf("readdir walk of %s was incomplete", snap.ID)
		}

		norm := func(es []manifestEntry) []string {
			var out []string
			for _, e := range es {
				out = append(out, fmt.Sprintf("%s|%d|%d|%d", e.Rel, e.Ino, e.Size, e.MtimeNs))
			}
			sort.Strings(out)
			return out
		}
		if a, b := norm(tree), norm(readdir); !reflect.DeepEqual(a, b) {
			t.Fatalf("snapshot %s: the two walks disagree\n tree:    %v\n readdir: %v", snap.ID, a, b)
		}
	}
}

// Deleting a file outside the tool is the normal case, and nothing about it is
// cached. A warm cache must not hide a newly deleted file.
func TestIntegrationCacheSeesLiveDeletionsMadeOutsideTheTool(t *testing.T) {
	requireIntegration(t)
	fs, pair := buildCacheTestFS(t)

	dirs := Dirs{State: t.TempDir(), Runtime: t.TempDir()}
	c := OpenCache(dirs, CacheOptions{})
	before := summarise(scanWith(t, pair, ScanOptions{Cache: c, CacheFloor: 1 << 20}))
	c.Close()

	for _, s := range before {
		if strings.HasPrefix(s, "keep.iso|") {
			t.Fatalf("keep.iso is still live and must not be a candidate: %v", before)
		}
	}

	// The user deletes a live file by hand, with the tool nowhere in sight.
	if err := os.Remove(filepath.Join(fs.path("live"), "keep.iso")); err != nil {
		t.Fatal(err)
	}
	unix.Sync()

	warm := OpenCache(dirs, CacheOptions{})
	after := summarise(scanWith(t, pair, ScanOptions{Cache: warm, CacheFloor: 1 << 20}))
	hits := warm.Stats.ManifestHits.Load()
	warm.Close()

	if hits == 0 {
		t.Error("precondition: the second scan should still have reused the snapshot listings")
	}
	var found bool
	for _, s := range after {
		if strings.HasPrefix(s, "keep.iso|"+KindDeleted+"|") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a file deleted outside the tool was not reported: %v", after)
	}
}

// If a snapshot is modified behind the tool's back, its cached listing must not
// be reused. CTransID moves when the tree changes, which is what makes this
// safe without any explicit invalidation.
func TestIntegrationModifiedSnapshotInvalidatesItsCacheEntry(t *testing.T) {
	requireIntegration(t)
	fs, pair := buildCacheTestFS(t)
	_ = fs

	dirs := Dirs{State: t.TempDir(), Runtime: t.TempDir()}
	c := OpenCache(dirs, CacheOptions{})
	_ = scanWith(t, pair, ScanOptions{Cache: c, CacheFloor: 1 << 20})
	c.Close()

	// Someone clears the read-only flag and deletes inside the snapshot.
	victim := pair.Snapshots[0]
	if err := SetSubvolReadOnly(victim.Root, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(victim.Root, "junk.iso")); err != nil {
		t.Fatal(err)
	}
	if err := SetSubvolReadOnly(victim.Root, true); err != nil {
		t.Fatal(err)
	}
	unix.Sync()

	// Re-detect, because the modification moved the subvolume's CTransID and
	// that is precisely what the cache key must now disagree with.
	repaired, err := DetectGeneric(fs.path("live"), fs.path("snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	var after Snapshot
	for _, s := range repaired.Snapshots {
		if s.UUID == victim.UUID {
			after = s
		}
	}
	if after.CTransID == victim.CTransID {
		t.Fatalf("CTransID did not move after modifying snapshot %s; the cache key would be stale", victim.ID)
	}

	warm := OpenCache(dirs, CacheOptions{})
	defer warm.Close()
	if _, ok := warm.Manifest(fsIDOf(t, repaired.Live), after, 1<<20); ok {
		t.Error("a modified snapshot must not reuse its old listing")
	}

	cands := scanWith(t, repaired, ScanOptions{Cache: warm, CacheFloor: 1 << 20})
	for _, cand := range cands {
		if cand.RelPath != "junk.iso" {
			continue
		}
		if len(cand.Copies) != 2 {
			t.Errorf("junk.iso reported in %d snapshots, want 2 after one was emptied", len(cand.Copies))
		}
	}
}

// A file removed from every snapshot must disappear from the next scan, and its
// cached listings must go with it.
func TestIntegrationPurgeInvalidatesTheCache(t *testing.T) {
	requireIntegration(t)
	fs, pair := buildCacheTestFS(t)

	dirs := Dirs{State: t.TempDir(), Runtime: t.TempDir()}
	c := OpenCache(dirs, CacheOptions{})
	cands := scanWith(t, pair, ScanOptions{Cache: c, CacheFloor: 1 << 20, CostLimit: 0})
	c.Close()

	var target Candidate
	for _, cand := range cands {
		if cand.RelPath == "junk.iso" {
			target = cand
		}
	}
	if target.ID == 0 {
		t.Fatalf("junk.iso was not found: %+v", summarise(cands))
	}

	st := &ScanState{Pairs: []Pair{pair}, Candidates: cands, ScannedAt: time.Now()}
	plan, err := BuildPlan(st, []int{target.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(dirs.State, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := (&Purger{Journal: journal, Out: io.Discard}).Execute(plan); err != nil {
		t.Fatal(err)
	}
	unix.Sync()

	// What cmdPurge does after a successful run.
	fsID := fsIDOf(t, pair.Live)
	dropper := OpenCache(dirs, CacheOptions{})
	for _, cp := range target.Copies {
		dropper.DropSnapshot(fsID, cp.SnapshotUUID)
	}
	dropper.Close()

	repaired, err := DetectGeneric(fs.path("live"), fs.path("snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	warm := OpenCache(dirs, CacheOptions{})
	defer warm.Close()
	for _, s := range repaired.Snapshots {
		if _, ok := warm.Manifest(fsID, s, 1<<20); ok {
			t.Errorf("snapshot %s still has a cached listing after a purge", s.ID)
		}
	}
	for _, cand := range scanWith(t, repaired, ScanOptions{Cache: warm, CacheFloor: 1 << 20}) {
		if cand.RelPath == "junk.iso" {
			t.Errorf("junk.iso is still reported after being purged from every snapshot")
		}
	}
}

// One manifest has to serve scans with different thresholds and different
// excludes, or the cache would only ever help a repeat of the identical
// command.
func TestIntegrationOneManifestServesDifferentFlags(t *testing.T) {
	requireIntegration(t)
	fs, pair := buildCacheTestFS(t)
	_ = fs

	dirs := Dirs{State: t.TempDir(), Runtime: t.TempDir()}
	c := OpenCache(dirs, CacheOptions{})
	defer c.Close()

	// Record once at a low floor.
	_ = scanWith(t, pair, ScanOptions{Cache: c, MinSize: 1 << 20, CacheFloor: 1 << 20})
	stored := c.Stats.ManifestMisses.Load()
	if stored == 0 {
		t.Fatal("precondition: the first scan must have walked")
	}

	// A higher threshold reuses it.
	high := summarise(scanWith(t, pair, ScanOptions{Cache: c, MinSize: 30 << 20, CacheFloor: 1 << 20}))
	for _, s := range high {
		if strings.HasPrefix(s, "deep/nested/buried.bin|") {
			t.Errorf("--min-size 30M must exclude the 24M file: %v", high)
		}
	}

	// So does an exclude, and it must actually filter.
	excluded := summarise(scanWith(t, pair, ScanOptions{
		Cache: c, MinSize: 1 << 20, CacheFloor: 1 << 20, Excludes: []string{"deep/*"},
	}))
	for _, s := range excluded {
		if strings.HasPrefix(s, "deep/") {
			t.Errorf("--exclude was not applied to a cached listing: %v", excluded)
		}
	}
	if c.Stats.ManifestHits.Load() != int64(2*len(pair.Snapshots)) {
		t.Errorf("reused %d listings across two further scans, want %d",
			c.Stats.ManifestHits.Load(), 2*len(pair.Snapshots))
	}
}

func fsIDOf(t *testing.T, path string) string {
	t.Helper()
	id, err := FilesystemID(path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
