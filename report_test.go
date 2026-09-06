package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Relocate is what makes a saved scan usable by a later purge, because the
// snapshots are normally reached through a mount point that differs per run.
func TestRelocateRewritesStalePaths(t *testing.T) {
	st := &ScanState{
		Pairs: []Pair{{Name: "@home", Live: "/home", Snapshots: []Snapshot{
			{ID: "2026-09-01", Root: "/run/old-mount/snapshots/2026-09-01/@home"},
			{ID: "2026-09-02", Root: "/run/old-mount/snapshots/2026-09-02/@home"},
		}}},
		Candidates: []Candidate{{
			ID: 1, Pair: "@home", RelPath: "user/big.iso",
			LivePath: "/home/user/big.iso",
			Copies: []Copy{
				{SnapshotID: "2026-09-01", Snapshot: "/run/old-mount/snapshots/2026-09-01/@home",
					Path: "/run/old-mount/snapshots/2026-09-01/@home/user/big.iso"},
				{SnapshotID: "2026-09-02", Snapshot: "/run/old-mount/snapshots/2026-09-02/@home",
					Path: "/run/old-mount/snapshots/2026-09-02/@home/user/big.iso"},
			},
		}},
	}

	current := []Pair{{Name: "@home", Live: "/home", Snapshots: []Snapshot{
		{ID: "2026-09-01", Root: "/run/new-mount/snapshots/2026-09-01/@home"},
		{ID: "2026-09-02", Root: "/run/new-mount/snapshots/2026-09-02/@home"},
	}}}

	if dropped := st.Relocate(current); dropped != 0 {
		t.Fatalf("dropped %d copies, want 0", dropped)
	}
	for _, cp := range st.Candidates[0].Copies {
		if !strings.HasPrefix(cp.Path, "/run/new-mount/") {
			t.Errorf("stale path survived relocation: %s", cp.Path)
		}
		want := filepath.Join(cp.Snapshot, "user/big.iso")
		if cp.Path != want {
			t.Errorf("path = %s, want %s", cp.Path, want)
		}
	}
}

// A snapshot that rotated away between scan and purge must be dropped, not
// carried forward as a path that no longer resolves.
func TestRelocateDropsRotatedSnapshots(t *testing.T) {
	st := &ScanState{
		Candidates: []Candidate{{
			ID: 1, Pair: "@home", RelPath: "big.iso",
			Copies: []Copy{
				{SnapshotID: "old", Snapshot: "/mnt/a"},
				{SnapshotID: "still-here", Snapshot: "/mnt/b"},
			},
		}},
	}
	current := []Pair{{Name: "@home", Live: "/home", Snapshots: []Snapshot{
		{ID: "still-here", Root: "/new/b"},
	}}}

	dropped := st.Relocate(current)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(st.Candidates[0].Copies) != 1 || st.Candidates[0].Copies[0].SnapshotID != "still-here" {
		t.Fatalf("copies = %+v", st.Candidates[0].Copies)
	}
}

// A candidate past --cost-limit was never measured; it must not be shown as a
// zero-byte win, which would read as "this frees nothing".
func TestRenderTableMarksUnmeasuredCandidates(t *testing.T) {
	st := &ScanState{
		ScannedAt: time.Now(),
		MinSize:   1 << 20,
		Costed:    1,
		Candidates: []Candidate{
			{ID: 1, Pair: "@home", RelPath: "measured.iso", Apparent: 100 << 20, TotalIn: 2,
				Copies: []Copy{{}, {}},
				Usage:  SetUsage{Bytes: 100 << 20, Method: MethodTreeSearch, Exact: true}},
			{ID: 2, Pair: "@home", RelPath: "unmeasured.iso", Apparent: 50 << 20, TotalIn: 2,
				Copies: []Copy{{}, {}},
				Usage:  SetUsage{Method: MethodNone}},
		},
	}
	var buf bytes.Buffer
	RenderTable(&buf, st, 0, true)
	out := buf.String()

	if !strings.Contains(out, "measured.iso") || !strings.Contains(out, "unmeasured.iso") {
		t.Fatalf("both rows should render:\n%s", out)
	}
	if !strings.Contains(out, "?") {
		t.Errorf("an unmeasured candidate must be shown as unknown, not as 0 B:\n%s", out)
	}
	if strings.Contains(out, "1 row(s) marked ~ are estimates") {
		t.Errorf("an unmeasured row must not be described as a ~ estimate:\n%s", out)
	}
	if !strings.Contains(out, "were not measured") {
		t.Errorf("the footer must explain the ? rows:\n%s", out)
	}
	if !strings.Contains(out, "Only the 1 largest of 2 candidates were measured") {
		t.Errorf("the footer must say the list was not fully measured:\n%s", out)
	}
}

func TestRenderTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	RenderTable(&buf, &ScanState{MinSize: 50 << 20}, 0, false)
	if !strings.Contains(buf.String(), "No pinned files found") {
		t.Errorf("got %q", buf.String())
	}
}

func TestRenderTableStatesTheAllHoldersRule(t *testing.T) {
	st := &ScanState{
		MinSize: 1 << 20, Costed: 1,
		Candidates: []Candidate{{ID: 1, Pair: "@home", RelPath: "a.iso", TotalIn: 3,
			Copies: []Copy{{}, {}, {}}, Usage: SetUsage{Bytes: 1 << 20, Method: MethodTreeSearch, Exact: true}}},
	}
	var buf bytes.Buffer
	RenderTable(&buf, st, 0, false)
	// The single most misleading thing this tool could do is imply that
	// removing one copy frees the space, so the output must say otherwise.
	if !strings.Contains(buf.String(), "ALL the snapshots") {
		t.Errorf("footer must explain the all-holders rule:\n%s", buf.String())
	}
}

func TestSaveAndLoadStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := &ScanState{
		ScannedAt: time.Now().Truncate(time.Second),
		MinSize:   50 << 20,
		Pairs:     []Pair{{Name: "@home", Live: "/home", Snapshots: []Snapshot{{ID: "s1", Root: "/x"}}}},
		Candidates: []Candidate{{ID: 1, Pair: "@home", RelPath: "f.iso",
			Copies: []Copy{{SnapshotID: "s1", Ino: 42, Size: 99, MtimeNs: 1234}},
			Usage:  SetUsage{Bytes: 99, Method: MethodTreeSearch, Exact: true}}},
	}
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Copies[0].Ino != 42 {
		t.Fatalf("fingerprints did not survive the round trip: %+v", got.Candidates)
	}
	if got.Candidates[0].Usage.Method != MethodTreeSearch {
		t.Errorf("method lost: %+v", got.Candidates[0].Usage)
	}
}

func TestLoadStateRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeFile(path, `{"version": 999}`); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("a state file from another version must be rejected")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// A row whose RECLAIM exceeds its APPARENT size reads as a bug unless the
// output explains it. It happens for real: a repeatedly rewritten VM image
// references parts of larger extents whose dead remainder is still allocated.
func TestRenderTableExplainsReclaimAboveApparent(t *testing.T) {
	st := &ScanState{
		MinSize: 1 << 20, Costed: 1,
		Candidates: []Candidate{{
			ID: 1, Pair: "@home", RelPath: "vm/disk.qcow2", TotalIn: 7,
			Apparent: 4270 << 20,
			Copies:   []Copy{{}},
			Usage:    SetUsage{Bytes: 5370 << 20, Method: MethodTreeSearch, Exact: true},
		}},
	}
	var buf bytes.Buffer
	RenderTable(&buf, st, 0, true)
	if !strings.Contains(buf.String(), "RECLAIM above APPARENT") {
		t.Errorf("output must explain why reclaim exceeds the file size:\n%s", buf.String())
	}
}

func TestRenderTableNoOverApparentNoteWhenNotApplicable(t *testing.T) {
	st := &ScanState{
		MinSize: 1 << 20, Costed: 1,
		Candidates: []Candidate{{
			ID: 1, Pair: "@home", RelPath: "a.iso", TotalIn: 2, Apparent: 100 << 20,
			Copies: []Copy{{}}, Usage: SetUsage{Bytes: 90 << 20, Method: MethodTreeSearch, Exact: true},
		}},
	}
	var buf bytes.Buffer
	RenderTable(&buf, st, 0, true)
	if strings.Contains(buf.String(), "RECLAIM above APPARENT") {
		t.Error("the note must not appear when no row exceeds its apparent size")
	}
}

// The over-apparent note must describe what the reader can see. Extent
// rounding makes most files exceed their apparent size by a few bytes, which
// the formatted column hides; counting those made the note contradict the
// table (it claimed six rows when only one looked that way).
func TestRenderTableOverApparentNoteCountsOnlyVisibleRows(t *testing.T) {
	st := &ScanState{
		MinSize: 1 << 20, Costed: 3,
		Candidates: []Candidate{
			// Visibly larger: 609 MiB vs 608 MiB.
			{ID: 1, Pair: "@home", RelPath: "visible.mkv", TotalIn: 7, Apparent: 608 << 20,
				Copies: []Copy{{}}, Usage: SetUsage{Bytes: 609 << 20, Method: MethodTreeSearch, Exact: true}},
			// Larger by a few bytes only; both format as "681 MiB".
			{ID: 2, Pair: "@home", RelPath: "rounding.jar", TotalIn: 7, Apparent: 681 << 20,
				Copies: []Copy{{}}, Usage: SetUsage{Bytes: 681<<20 + 4096, Method: MethodTreeSearch, Exact: true}},
			{ID: 3, Pair: "@home", RelPath: "rounding2.jar", TotalIn: 7, Apparent: 617 << 20,
				Copies: []Copy{{}}, Usage: SetUsage{Bytes: 617<<20 + 512, Method: MethodTreeSearch, Exact: true}},
		},
	}
	var buf bytes.Buffer
	RenderTable(&buf, st, 0, true)
	out := buf.String()

	if !strings.Contains(out, "1 row(s) show RECLAIM above APPARENT") {
		t.Errorf("expected the note to count only the one visibly larger row:\n%s", out)
	}
	if strings.Contains(out, "3 row(s) show RECLAIM above APPARENT") {
		t.Errorf("rows hidden by rounding must not be counted:\n%s", out)
	}
}

// folderState builds a folder-view scan result for the renderer tests.
func folderState() *ScanState {
	return &ScanState{
		FolderMinSize: 50 << 20,
		FreeBytes:     10 << 30,
		Costed:        2,
		Folders: []Folder{
			{
				ID: 1, Pair: "@home", RelPath: "user/projects/old-app",
				Kind: FolderDeleted, Files: 48213, CopyN: 96426, Apparent: 5 << 30,
				Usage:   SetUsage{Bytes: 4 << 30, Method: MethodSampled},
				TotalIn: 8,
				Holders: []FolderHolder{{SnapshotID: "a"}, {SnapshotID: "b"}},
			},
			{
				ID: 2, Pair: "@home", RelPath: "user/Downloads",
				Kind: FolderThinned, Files: 12, CopyN: 24, Apparent: 900 << 20,
				Usage:   SetUsage{Bytes: 800 << 20, Method: MethodTreeSearch, Exact: true},
				TotalIn: 8,
				Holders: []FolderHolder{{SnapshotID: "a"}},
			},
		},
	}
}

func TestRenderFolderTableShowsFileCountsAndFolderIDs(t *testing.T) {
	var buf bytes.Buffer
	RenderFolderTable(&buf, folderState(), 0, true)
	out := buf.String()

	for _, want := range []string{
		"FILES",
		"F1", "F2",
		"48213",
		"user/projects/old-app",
		"deleted", "thinned",
		"2 folder(s) holding 48225 file(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("folder table is missing %q:\n%s", want, out)
		}
	}
}

// A sampled figure is an estimate and must both carry the ~ marker and say why.
func TestRenderFolderTableMarksSampledRows(t *testing.T) {
	var buf bytes.Buffer
	RenderFolderTable(&buf, folderState(), 0, true)
	out := buf.String()

	if !strings.Contains(out, "~4.00 GiB") {
		t.Errorf("a sampled row must be marked with ~:\n%s", out)
	}
	if !strings.Contains(out, "evenly spaced sample") {
		t.Errorf("the notes must explain the sampling:\n%s", out)
	}
	if !strings.Contains(out, "--folder-sample") {
		t.Errorf("the notes must name the flag that narrows it:\n%s", out)
	}
	if !strings.Contains(out, "thinned") || !strings.Contains(out, "leaves the live directory untouched") {
		t.Errorf("the notes must explain what a thinned row means:\n%s", out)
	}
}

func TestRenderFolderTableHonoursTop(t *testing.T) {
	var buf bytes.Buffer
	RenderFolderTable(&buf, folderState(), 1, true)
	out := buf.String()
	if strings.Contains(out, "user/Downloads") {
		t.Errorf("--top 1 showed a second row:\n%s", out)
	}
	if !strings.Contains(out, "1 more not shown") {
		t.Errorf("the reader must be told rows were held back:\n%s", out)
	}
}

func TestRenderFolderTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	RenderFolderTable(&buf, &ScanState{FolderMinSize: 50 << 20}, 0, false)
	if !strings.Contains(buf.String(), "No pinned folders found at or above 50.0 MiB") {
		t.Errorf("unexpected empty rendering: %q", buf.String())
	}
}

// Folder holders are recorded under a mount point that differs on every run,
// exactly as candidate copies are, so they need the same relocation.
func TestRelocateRewritesFolderHolders(t *testing.T) {
	st := &ScanState{
		FolderMinSize: 1,
		Folders: []Folder{{
			ID: 1, Pair: "@home", RelPath: "user/projects",
			Holders: []FolderHolder{
				{SnapshotID: "2026-09-01", Root: "/run/old-mount/snapshots/2026-09-01/@home"},
				{SnapshotID: "gone", Root: "/run/old-mount/snapshots/gone/@home"},
			},
		}},
	}
	current := []Pair{{Name: "@home", Live: "/home", Snapshots: []Snapshot{
		{ID: "2026-09-01", Root: "/run/new-mount/snapshots/2026-09-01/@home"},
	}}}

	if dropped := st.Relocate(current); dropped != 1 {
		t.Fatalf("dropped %d holders, want 1 (the rotated snapshot)", dropped)
	}
	f := st.Folders[0]
	if len(f.Holders) != 1 || f.Holders[0].Root != "/run/new-mount/snapshots/2026-09-01/@home" {
		t.Errorf("holders not relocated: %+v", f.Holders)
	}
	if f.Live != "/home" || f.LivePath != "/home/user/projects" {
		t.Errorf("live path not refreshed: live=%q livePath=%q", f.Live, f.LivePath)
	}
}

// A folder of many small files routinely reclaims more than the file sizes add
// up to, because each file occupies whole blocks. Unexplained, that reads as a
// bug in the tool rather than as how the filesystem works.
func TestRenderFolderTableExplainsReclaimAboveApparent(t *testing.T) {
	st := folderState()
	st.Folders[0].Apparent = 83 << 20
	st.Folders[0].Usage = SetUsage{Bytes: 89 << 20, Method: MethodTreeSearch, Exact: true}

	var buf bytes.Buffer
	RenderFolderTable(&buf, st, 0, true)
	out := buf.String()
	if !strings.Contains(out, "RECLAIM above APPARENT") {
		t.Errorf("the notes must explain a row that reclaims more than its apparent size:\n%s", out)
	}
	if !strings.Contains(out, "whole blocks") {
		t.Errorf("the explanation must give the reason:\n%s", out)
	}
}

// The note must not fire when the two columns render identically, or it
// contradicts the table it is explaining.
func TestRenderFolderTableStaysQuietOnInvisibleRounding(t *testing.T) {
	st := folderState()
	st.Folders[0].Apparent = 89 << 20
	st.Folders[0].Usage = SetUsage{Bytes: 89<<20 + 512, Method: MethodTreeSearch, Exact: true}
	st.Folders[1].Usage = SetUsage{Bytes: 1, Method: MethodTreeSearch, Exact: true}

	var buf bytes.Buffer
	RenderFolderTable(&buf, st, 0, true)
	if out := buf.String(); strings.Contains(out, "RECLAIM above APPARENT") {
		t.Errorf("the note fired on a difference the reader cannot see:\n%s", out)
	}
}
