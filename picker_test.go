package main

import (
	"strings"
	"testing"
)

// newTestPicker builds a picker over rows without going through RunPicker,
// which needs a terminal on stdin.
func newTestPicker(rows []pickRow) *picker {
	p := &picker{rows: rows, selected: map[int]bool{}}
	for _, r := range rows {
		if r.Files > 0 {
			p.showFiles = true
			break
		}
	}
	p.applyFilter()
	return p
}

func testRows() []pickRow {
	return []pickRow{
		{ID: 1, Bytes: 1 << 30, Holders: 4, TotalIn: 8, Pair: "@home", Path: "user/a.iso"},
		{ID: 2, Bytes: 2 << 30, Holders: 8, TotalIn: 8, Pair: "@home", Path: "user/b.iso"},
		{ID: 3, Bytes: 3 << 30, Holders: 2, TotalIn: 8, Pair: "@root", Path: "var/c.iso"},
	}
}

func TestCandidateRowsCarryTheMeasurementState(t *testing.T) {
	rows := CandidateRows([]Candidate{
		{ID: 1, Pair: "@home", RelPath: "a.iso", TotalIn: 8,
			Copies: []Copy{{}, {}},
			Usage:  SetUsage{Bytes: 100, Method: MethodTreeSearch, Exact: true}},
		{ID: 2, Pair: "@home", RelPath: "b.iso", TotalIn: 8,
			Usage: SetUsage{Method: MethodNone}},
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Approx || rows[0].Unknown || rows[0].Holders != 2 {
		t.Errorf("an exact row was flattened wrongly: %+v", rows[0])
	}
	if !rows[1].Unknown {
		t.Errorf("an unmeasured row must be marked unknown: %+v", rows[1])
	}
	if rows[0].Files != 0 {
		t.Error("a file row must not claim a file count")
	}
}

func TestFolderRowsCarryFileCountsAndFlagThinned(t *testing.T) {
	rows := FolderRows([]Folder{
		{ID: 1, Pair: "@home", RelPath: "user/projects", Kind: FolderDeleted, Files: 4821,
			TotalIn: 8, Holders: []FolderHolder{{}, {}},
			Usage: SetUsage{Bytes: 1 << 30, Method: MethodSampled}},
		{ID: 2, Pair: "@home", RelPath: "user/Downloads", Kind: FolderThinned, Files: 12,
			TotalIn: 8, Holders: []FolderHolder{{}},
			Usage: SetUsage{Bytes: 1 << 20, Method: MethodTreeSearch, Exact: true}},
	})
	if rows[0].Files != 4821 || rows[0].Holders != 2 {
		t.Errorf("folder row flattened wrongly: %+v", rows[0])
	}
	if !rows[0].Approx {
		t.Error("a sampled folder must render as an estimate")
	}
	if !strings.HasSuffix(rows[0].Path, "/") {
		t.Errorf("a folder row should read as a directory, got %q", rows[0].Path)
	}
	// Whether the live directory survives is the difference between the two
	// kinds, and the picker has no KIND column to show it in.
	if !strings.Contains(rows[1].Path, "thinned") {
		t.Errorf("a thinned folder must say so in the list, got %q", rows[1].Path)
	}
}

// "a" has to be a toggle: pressing it on a list of tens of thousands of rows
// by accident should be undone by pressing it again, not by clearing
// everything and starting over.
func TestPickerSelectAllShownIsAToggle(t *testing.T) {
	p := newTestPicker(testRows())

	p.handle([]byte{'a'})
	if p.countSelected() != 3 {
		t.Fatalf("a selected %d rows, want all 3", p.countSelected())
	}
	p.handle([]byte{'a'})
	if p.countSelected() != 0 {
		t.Fatalf("a did not toggle back off, %d still selected", p.countSelected())
	}
}

// With a filter on, "a" means the rows in front of you and "A" means all of
// them. Conflating the two is how a selection quietly grows.
func TestPickerSelectAllRespectsAndEscapesTheFilter(t *testing.T) {
	p := newTestPicker(testRows())
	p.filter = "var/"
	p.applyFilter()
	if len(p.view) != 1 {
		t.Fatalf("filter matched %d rows, want 1", len(p.view))
	}

	p.handle([]byte{'a'})
	if p.countSelected() != 1 || !p.selected[3] {
		t.Errorf("a selected %d rows, want only the filtered one", p.countSelected())
	}

	p.handle([]byte{'A'})
	if p.countSelected() != 3 {
		t.Errorf("A selected %d rows, want every row regardless of the filter", p.countSelected())
	}

	// "a" now sees its shown row already ticked, so it clears the shown one
	// and leaves the rest of the selection alone.
	p.handle([]byte{'a'})
	if p.selected[3] {
		t.Error("a should have cleared the filtered row")
	}
	if !p.selected[1] || !p.selected[2] {
		t.Error("a must not touch rows the filter is hiding")
	}
}

func TestPickerNoneClearsEverything(t *testing.T) {
	p := newTestPicker(testRows())
	p.handle([]byte{'A'})
	p.handle([]byte{'n'})
	if p.countSelected() != 0 {
		t.Errorf("n left %d rows selected", p.countSelected())
	}
}

func TestPickerToggleAndResultReturnIDs(t *testing.T) {
	p := newTestPicker(testRows())
	p.handle([]byte{' '}) // row 1
	p.handle([]byte{'j'}) // down
	p.handle([]byte{'j'}) // down
	p.handle([]byte{' '}) // row 3
	got := p.result()
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("result = %v, want [1 3]", got)
	}
}

func TestPickerAllShownSelectedOnAnEmptyView(t *testing.T) {
	p := newTestPicker(testRows())
	p.filter = "nothing-matches-this"
	p.applyFilter()
	if p.allShownSelected() {
		t.Error("an empty view is not 'all selected'; a would then clear instead of select")
	}
	p.handle([]byte{'a'})
	if p.countSelected() != 0 {
		t.Error("a on an empty view must select nothing")
	}
}

func TestPickerRowTextShowsFileCountsOnlyForFolders(t *testing.T) {
	files := newTestPicker(testRows())
	if got := files.text(files.rows[0]); strings.Contains(got, "files") {
		t.Errorf("a file row grew a file count: %q", got)
	}

	folders := newTestPicker(FolderRows([]Folder{
		{ID: 1, Pair: "@home", RelPath: "user/projects", Kind: FolderDeleted, Files: 4821,
			TotalIn: 8, Holders: []FolderHolder{{}, {}},
			Usage: SetUsage{Bytes: 1 << 30, Method: MethodSampled}},
	}))
	got := folders.text(folders.rows[0])
	for _, want := range []string{"4821 files", "~1.00 GiB", "2/8", "@home", "user/projects/"} {
		if !strings.Contains(got, want) {
			t.Errorf("folder row %q is missing %q", got, want)
		}
	}
}

func TestPickerRowTextMarksUnmeasuredRows(t *testing.T) {
	p := newTestPicker([]pickRow{{ID: 1, Unknown: true, Pair: "@home", Path: "a.iso"}})
	if got := p.text(p.rows[0]); !strings.Contains(got, "?") {
		t.Errorf("an unmeasured row must render as ?, got %q", got)
	}
}

func TestPickerFilterEditing(t *testing.T) {
	p := newTestPicker(testRows())
	p.handle([]byte{'/'})
	if !p.filtered {
		t.Fatal("/ did not enter filter mode")
	}
	p.handle([]byte("var"))
	if len(p.view) != 1 {
		t.Errorf("filter %q matched %d rows, want 1", p.filter, len(p.view))
	}
	p.handle([]byte{0x7f}) // backspace
	if p.filter != "va" {
		t.Errorf("filter = %q, want %q", p.filter, "va")
	}
	p.handle([]byte{'\r'})
	if p.filtered {
		t.Error("enter did not leave filter mode")
	}
}

func TestPickerQuitAndConfirm(t *testing.T) {
	p := newTestPicker(testRows())
	if _, abort := p.handle([]byte{'q'}); !abort {
		t.Error("q must abort")
	}
	if done, _ := p.handle([]byte{'\r'}); !done {
		t.Error("enter must confirm")
	}
}
