package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mkfile writes a file of the given size, creating its parents.
func mkfile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The rollup's whole reason to exist: a deleted tree must be reported once, at
// its top, not once per subdirectory inside it.
func TestFolderKeyRollsUpToTopmostMissingDirectory(t *testing.T) {
	live := t.TempDir()
	if err := os.MkdirAll(filepath.Join(live, "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	// live/user exists; live/user/projects and everything under it does not.

	probe := newLiveProbe()
	for _, rel := range []string{
		"user/projects/app/node_modules/react/index.js",
		"user/projects/app/src/main.go",
		"user/projects/other/deep/nested/thing.bin",
	} {
		dir, deleted := folderKey(live, rel, probe)
		if !deleted {
			t.Errorf("%s: should be reported as a deleted tree", rel)
		}
		if dir != "user/projects" {
			t.Errorf("%s rolled up to %q, want the topmost missing directory %q",
				rel, dir, "user/projects")
		}
	}
}

func TestFolderKeyUsesParentWhenEveryAncestorLives(t *testing.T) {
	live := t.TempDir()
	if err := os.MkdirAll(filepath.Join(live, "user/Downloads"), 0o755); err != nil {
		t.Fatal(err)
	}

	dir, deleted := folderKey(live, "user/Downloads/old.iso", newLiveProbe())
	if deleted {
		t.Error("a directory that still exists live must not be reported as deleted")
	}
	if dir != "user/Downloads" {
		t.Errorf("got %q, want %q", dir, "user/Downloads")
	}
}

func TestFolderKeyRootLevelFile(t *testing.T) {
	dir, deleted := folderKey(t.TempDir(), "loose.bin", newLiveProbe())
	if dir != "." || deleted {
		t.Errorf("got (%q, %v), want (\".\", false): the subvolume root always exists", dir, deleted)
	}
}

// A directory can be missing at one level and present at a deeper one only if
// the shallower one is gone, so the shallowest miss is the right answer even
// when a deeper ancestor happens to exist under a different live path.
func TestFolderKeyStopsAtTheFirstMissingAncestor(t *testing.T) {
	live := t.TempDir()
	if err := os.MkdirAll(filepath.Join(live, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, deleted := folderKey(live, "a/b/c/d.bin", newLiveProbe())
	if !deleted || dir != "a/b" {
		t.Errorf("got (%q, %v), want (\"a/b\", true)", dir, deleted)
	}
}

// cand builds a candidate the rollup can group, with one copy per snapshot id.
func cand(live, pair, rel string, apparent uint64, snaps ...string) Candidate {
	c := Candidate{
		Pair: pair, Live: live, RelPath: rel, LivePath: filepath.Join(live, rel),
		Kind: KindDeleted, Apparent: apparent, TotalIn: 8,
	}
	for _, s := range snaps {
		c.Copies = append(c.Copies, Copy{
			SnapshotID: s,
			Snapshot:   "/snaps/" + s,
			Path:       filepath.Join("/snaps", s, rel),
			Size:       apparent,
		})
	}
	return c
}

// The case the folder view exists for: many small files nobody would ever see
// in a file list, adding up to a folder that is worth reclaiming.
func TestRollupFoldersSumsManySmallFiles(t *testing.T) {
	live := t.TempDir()
	// live/user survives; everything below it was deleted.
	if err := os.MkdirAll(filepath.Join(live, "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	var cands []Candidate
	for i := 0; i < 1000; i++ {
		cands = append(cands, cand(live, "@home",
			filepath.Join("user/projects/app/node_modules/pkg", string(rune('a'+i%26)), "index.js"),
			30<<10, "2026-09-01", "2026-09-02"))
	}

	r := rollupFolders(cands, 20<<20)
	if len(r.Folders) != 1 {
		t.Fatalf("got %d folders, want 1: %+v", len(r.Folders), r.Folders)
	}
	f := r.Folders[0]
	if f.RelPath != "user/projects" {
		t.Errorf("RelPath = %q, want the topmost missing directory", f.RelPath)
	}
	if f.Kind != FolderDeleted {
		t.Errorf("Kind = %q, want %q", f.Kind, FolderDeleted)
	}
	if f.Files != 1000 {
		t.Errorf("Files = %d, want 1000", f.Files)
	}
	if want := uint64(1000 * 30 << 10); f.Apparent != want {
		t.Errorf("Apparent = %d, want %d", f.Apparent, want)
	}
	if f.CopyN != 2000 {
		t.Errorf("CopyN = %d, want 2000", f.CopyN)
	}
	if f.Snaps() != 2 || f.TotalIn != 8 {
		t.Errorf("SNAPS = %d/%d, want 2/8", f.Snaps(), f.TotalIn)
	}
	if len(r.Members) != 1 || len(r.Members[0]) != 1000 {
		t.Errorf("members not carried alongside the folder for measurement")
	}
	// Not one of those files would survive a file-list threshold.
	if f.Apparent/uint64(f.Files) >= 50<<20 {
		t.Fatal("the fixture is wrong: these files are not small")
	}
}

func TestRollupFoldersDropsRollupsBelowMinSize(t *testing.T) {
	live := t.TempDir()
	if err := os.MkdirAll(filepath.Join(live, "user"), 0o755); err != nil {
		t.Fatal(err)
	}
	cands := []Candidate{
		cand(live, "@home", "user/big/a.bin", 100<<20, "s1"),
		cand(live, "@home", "user/small/b.bin", 1<<20, "s1"),
	}
	r := rollupFolders(cands, 50<<20)
	if len(r.Folders) != 1 || r.Folders[0].RelPath != "user/big" {
		t.Fatalf("got %+v, want only user/big", r.Folders)
	}
	if len(r.Members) != len(r.Folders) {
		t.Fatalf("members list (%d) out of step with folders (%d)", len(r.Members), len(r.Folders))
	}
}

// Rel paths are relative to each pair's own live root, so the same path in two
// pairs is two different directories.
func TestRollupFoldersKeepsPairsApart(t *testing.T) {
	homeLive, rootLive := t.TempDir(), t.TempDir()
	cands := []Candidate{
		cand(homeLive, "@home", "cache/x.bin", 60<<20, "s1"),
		cand(rootLive, "@root", "cache/x.bin", 60<<20, "s1"),
	}
	r := rollupFolders(cands, 1)
	if len(r.Folders) != 2 {
		t.Fatalf("got %d folders, want one per pair: %+v", len(r.Folders), r.Folders)
	}
}

func TestRollupFoldersMarksLiveDirectoriesThinned(t *testing.T) {
	live := t.TempDir()
	if err := os.MkdirAll(filepath.Join(live, "user/Downloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := rollupFolders([]Candidate{
		cand(live, "@home", "user/Downloads/old.iso", 80<<20, "s1"),
	}, 1)
	if len(r.Folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(r.Folders))
	}
	if r.Folders[0].Kind != FolderThinned {
		t.Errorf("Kind = %q, want %q for a directory that still exists live",
			r.Folders[0].Kind, FolderThinned)
	}
}

func TestRankFoldersOrdersByReclaimAndNumbersFromOne(t *testing.T) {
	folders := []Folder{
		{RelPath: "small", Usage: SetUsage{Bytes: 10}},
		{RelPath: "big", Usage: SetUsage{Bytes: 900}},
		{RelPath: "tied-larger-apparent", Usage: SetUsage{Bytes: 10}, Apparent: 500},
	}
	rankFolders(folders)
	if folders[0].RelPath != "big" || folders[0].ID != 1 {
		t.Errorf("first row = %+v, want big with id 1", folders[0])
	}
	if folders[1].RelPath != "tied-larger-apparent" {
		t.Errorf("a tie on reclaim must break on apparent size, got %q", folders[1].RelPath)
	}
	if folders[2].ID != 3 {
		t.Errorf("ids = %d, want them numbered from 1", folders[2].ID)
	}
	if got := folders[0].Label(); got != "F1" {
		t.Errorf("Label() = %q, want F1", got)
	}
}

// measureFolder must not open every copy of a huge folder. The figures here
// depend on the filesystem the test runs on, so the assertions are about which
// path was taken and how much was read, not about the byte count.
func TestMeasureFolderSamplesPastTheBudget(t *testing.T) {
	dir := t.TempDir()
	var members []*Candidate
	for i := 0; i < 40; i++ {
		p := filepath.Join(dir, "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".bin")
		mkfile(t, p, 4096)
		members = append(members, &Candidate{
			Apparent: 4096,
			Copies:   []Copy{{Path: p, Size: 4096}, {Path: p, Size: 4096}},
		})
	}

	full, measured := measureFolder(members, 0, nil)
	if measured != 80 {
		t.Errorf("unbudgeted measurement read %d copies, want all 80", measured)
	}
	if full.Method == MethodSampled {
		t.Error("an unbudgeted measurement must not be marked as sampled")
	}

	sampled, read := measureFolder(members, 10, nil)
	if read > 12 {
		t.Errorf("sampled measurement read %d copies, want it held near the budget of 10", read)
	}
	if sampled.Method != MethodSampled {
		t.Fatalf("Method = %q, want %q", sampled.Method, MethodSampled)
	}
	if sampled.Exact {
		t.Error("a scaled sample is never exact")
	}
	if !sampled.Approx() {
		t.Error("a sampled row must render with a ~ marker")
	}
	// Whole files are sampled, never individual copies: splitting a file's
	// reflinked copies would destroy the sharing the union exists to measure.
	if read%2 != 0 {
		t.Errorf("read %d copies; sampling split a file's copy set", read)
	}
}

func TestMeasureFolderHandlesEmptyMembers(t *testing.T) {
	u, n := measureFolder(nil, 10, nil)
	if u.Method != MethodNone || n != 0 {
		t.Errorf("got (%+v, %d), want an unmeasured result", u, n)
	}
}

// snapFixture lays out a live subvolume and two snapshots of it, which is
// enough to exercise the folder expansion purge relies on. No btrfs and no
// root: the expansion is a directory walk plus a set of checks.
func snapFixture(t *testing.T) (live string, pairs []Pair, folder *Folder) {
	t.Helper()
	base := t.TempDir()
	live = filepath.Join(base, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}

	var snaps []Snapshot
	for i, id := range []string{"2026-09-01", "2026-09-02"} {
		root := filepath.Join(base, "snaps", id)
		mkfile(t, filepath.Join(root, "user/projects/app/src/main.go"), 1000)
		mkfile(t, filepath.Join(root, "user/projects/app/node_modules/a/index.js"), 2000)
		mkfile(t, filepath.Join(root, "user/projects/README.md"), 500)
		snaps = append(snaps, Snapshot{
			ID: id, Root: root, ReadOnly: true,
			UUID: "uuid-" + id, CTransID: uint64(100 + i),
		})
	}
	pairs = []Pair{{Provider: "test", Name: "@home", Live: live, Snapshots: snaps}}

	folder = &Folder{
		ID: 1, Provider: "test", Pair: "@home", Live: live,
		RelPath: "user/projects", LivePath: filepath.Join(live, "user/projects"),
		Kind: FolderDeleted, Files: 3, CopyN: 6, Apparent: 3500,
		Usage:   SetUsage{Bytes: 7000, Method: MethodTreeSearch, Exact: true},
		TotalIn: 2,
		Holders: []FolderHolder{
			{SnapshotID: "2026-09-01", Root: snaps[0].Root, UUID: "uuid-2026-09-01", CTransID: 100},
			{SnapshotID: "2026-09-02", Root: snaps[1].Root, UUID: "uuid-2026-09-02", CTransID: 101},
		},
	}
	return live, pairs, folder
}

func TestExpandFolderRebuildsEveryMemberFromTheSnapshots(t *testing.T) {
	_, pairs, folder := snapFixture(t)

	cands, skips, err := ExpandFolder(folder, pairs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 0 {
		t.Errorf("unexpected skips: %+v", skips)
	}
	if len(cands) != 3 {
		t.Fatalf("got %d candidates, want the folder's 3 files: %+v", len(cands), cands)
	}

	var rels []string
	for _, c := range cands {
		rels = append(rels, c.RelPath)
		if len(c.Copies) != 2 {
			t.Errorf("%s: %d copies, want one per holding snapshot", c.RelPath, len(c.Copies))
		}
		if c.Kind != KindDeleted {
			t.Errorf("%s: Kind = %q", c.RelPath, c.Kind)
		}
		if !strings.HasPrefix(c.RelPath, "user/projects/") {
			t.Errorf("%s: rebuilt path is not inside the folder", c.RelPath)
		}
		for _, cp := range c.Copies {
			if _, err := os.Lstat(cp.Path); err != nil {
				t.Errorf("%s: copy path does not exist: %v", c.RelPath, err)
			}
			if cp.Ino == 0 || cp.Size == 0 {
				t.Errorf("%s: copy was not fingerprinted (%+v)", c.RelPath, cp)
			}
		}
	}
	sort.Strings(rels)
	want := []string{
		"user/projects/README.md",
		"user/projects/app/node_modules/a/index.js",
		"user/projects/app/src/main.go",
	}
	if strings.Join(rels, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", rels, want)
	}

	// The shares must add back up to what the user was shown, because that
	// total is the number the dry run reports as the estimated reclaim.
	var sum uint64
	for _, c := range cands {
		sum += c.Usage.Bytes
		if c.Usage.Exact {
			t.Errorf("%s: a share of a set figure must not claim to be exact", c.RelPath)
		}
	}
	if diff := int64(sum) - int64(folder.Usage.Bytes); diff > 3 || diff < -3 {
		t.Errorf("shares sum to %d, want the folder's %d", sum, folder.Usage.Bytes)
	}
}

func TestExpandFolderAssignsIDsFromTheGivenStart(t *testing.T) {
	_, pairs, folder := snapFixture(t)
	cands, _, err := ExpandFolder(folder, pairs, 50)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cands {
		if c.ID != 50+i {
			t.Fatalf("candidate %d has id %d, want %d", i, c.ID, 50+i)
		}
	}
}

// The check that stands in for re-reading every file's mtime. A snapshot whose
// ctransid moved has changed since the scan, so the list rebuilt from it is no
// longer the list that was measured and shown.
func TestExpandFolderRefusesASnapshotThatChangedSinceTheScan(t *testing.T) {
	_, pairs, folder := snapFixture(t)
	pairs[0].Snapshots[1].CTransID = 999

	cands, skips, err := ExpandFolder(folder, pairs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "ctransid") {
		t.Fatalf("got skips %+v, want one naming the changed ctransid", skips)
	}
	for _, c := range cands {
		if len(c.Copies) != 1 {
			t.Errorf("%s: %d copies, want only the unchanged snapshot's", c.RelPath, len(c.Copies))
		}
	}
}

func TestExpandFolderRefusesADirectoryThatCameBack(t *testing.T) {
	live, pairs, folder := snapFixture(t)
	if err := os.MkdirAll(filepath.Join(live, "user/projects"), 0o755); err != nil {
		t.Fatal(err)
	}

	cands, skips, err := ExpandFolder(folder, pairs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("got %d candidates, want none once the directory exists live again", len(cands))
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "live filesystem") {
		t.Fatalf("got skips %+v, want one explaining the directory is back", skips)
	}
}

func TestExpandFolderSkipsAFileThatCameBack(t *testing.T) {
	live, pairs, folder := snapFixture(t)
	mkfile(t, filepath.Join(live, "user/projects/README.md"), 500)
	// The folder itself is thinned now, not deleted, which is what a rescan
	// would have recorded; the per-file check has to catch it regardless.
	folder.Kind = FolderThinned

	cands, skips, err := ExpandFolder(folder, pairs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Errorf("got %d candidates, want 2 with the returned file left alone", len(cands))
	}
	for _, c := range cands {
		if c.RelPath == "user/projects/README.md" {
			t.Error("a file that is back on the live tree must not be queued for removal")
		}
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "live filesystem") {
		t.Fatalf("got skips %+v, want one for the returned file", skips)
	}
}

func TestExpandFolderReportsARotatedSnapshot(t *testing.T) {
	_, pairs, folder := snapFixture(t)
	pairs[0].Snapshots = pairs[0].Snapshots[:1]

	cands, skips, err := ExpandFolder(folder, pairs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "rotated out") {
		t.Fatalf("got skips %+v, want one for the rotated snapshot", skips)
	}
	for _, c := range cands {
		if len(c.Copies) != 1 {
			t.Errorf("%s: want only the surviving snapshot's copy", c.RelPath)
		}
	}
}

func TestExpandFolderRejectsAnUnknownPair(t *testing.T) {
	_, pairs, folder := snapFixture(t)
	folder.Pair = "@nowhere"
	if _, _, err := ExpandFolder(folder, pairs, 1); err == nil {
		t.Fatal("expanding a folder whose pair is gone must fail loudly")
	}
}

// The whole folder view, end to end, on plain directories: no btrfs and no
// root. The walk, the rollup, the measurement and the table all run, which is
// what proves the view actually surfaces a folder made only of small files.
func TestFolderViewSurfacesATreeOfSmallFiles(t *testing.T) {
	base := t.TempDir()
	live := filepath.Join(base, "live")
	// Downloads survives on the live tree; projects was deleted outright.
	if err := os.MkdirAll(filepath.Join(live, "user/Downloads"), 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		files    = 300
		fileSize = 32 << 10
	)
	var snaps []Snapshot
	for i, id := range []string{"2026-09-01", "2026-09-02"} {
		root := filepath.Join(base, "snaps", id)
		for n := 0; n < files; n++ {
			mkfile(t, filepath.Join(root,
				fmt.Sprintf("user/projects/old-app/node_modules/pkg%d/index.js", n)), fileSize)
		}
		mkfile(t, filepath.Join(root, "user/Downloads/gone.iso"), 4<<20)
		snaps = append(snaps, Snapshot{
			ID: id, Root: root, ReadOnly: true,
			UUID: "uuid-" + id, CTransID: uint64(10 + i),
		})
	}
	pair := Pair{Provider: "test", Name: "@home", Live: live, Snapshots: snaps}

	// A file list at any sane threshold sees nothing here: every file in the
	// deleted tree is 32 KiB.
	plain, err := Scan([]Pair{pair}, ScanOptions{MinSize: 1 << 20, CostLimit: 0, Walk: WalkReaddir})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plain.Candidates {
		if strings.Contains(c.RelPath, "node_modules") {
			t.Fatalf("the file view should not have surfaced %s at a 1 MiB threshold", c.RelPath)
		}
	}

	res, err := Scan([]Pair{pair}, ScanOptions{
		MinSize:       0,
		Folders:       true,
		FolderMinSize: 1 << 20,
		CostLimit:     0,
		Walk:          WalkReaddir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("a folder scan should not carry %d candidates into its result", len(res.Candidates))
	}
	if len(res.Folders) != 2 {
		t.Fatalf("got %d folders, want the deleted tree and the thinned Downloads: %+v",
			len(res.Folders), res.Folders)
	}

	top := res.Folders[0]
	if top.RelPath != "user/projects" {
		t.Errorf("top row = %q, want the deleted tree rolled up to its top", top.RelPath)
	}
	if top.Kind != FolderDeleted {
		t.Errorf("top row Kind = %q, want %q", top.Kind, FolderDeleted)
	}
	if top.Files != files {
		t.Errorf("top row Files = %d, want %d", top.Files, files)
	}
	if want := uint64(files * fileSize); top.Apparent != want {
		t.Errorf("top row Apparent = %d, want %d", top.Apparent, want)
	}
	if top.Usage.Method == MethodNone || top.Usage.Bytes == 0 {
		t.Errorf("top row was not measured: %+v", top.Usage)
	}
	// It has to beat the 4 MiB file, which is the entire point of the view.
	if res.Folders[1].RelPath != "user/Downloads" {
		t.Errorf("second row = %q, want user/Downloads ranked below the tree", res.Folders[1].RelPath)
	}
	if res.Folders[1].Kind != FolderThinned {
		t.Errorf("Downloads Kind = %q, want %q", res.Folders[1].Kind, FolderThinned)
	}

	// And the rollup must be purgeable: the members rebuild from the snapshots.
	st := &ScanState{Pairs: []Pair{pair}, Folders: res.Folders, FolderMinSize: 1 << 20}
	cands, skips, err := ExpandFolder(&st.Folders[0], st.Pairs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 0 {
		t.Errorf("unexpected skips: %+v", skips)
	}
	if len(cands) != files {
		t.Errorf("expansion rebuilt %d files, want the %d the row promised", len(cands), files)
	}

	var buf bytes.Buffer
	RenderFolderTable(&buf, st, 0, true)
	if out := buf.String(); !strings.Contains(out, "user/projects") || !strings.Contains(out, "F1") {
		t.Errorf("folder table does not show the row:\n%s", out)
	}
}
