package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// pickRow is one selectable line, flattened from a candidate or from a folder
// rollup so the picker itself never needs to know which view it is showing.
type pickRow struct {
	ID      int    // returned to the caller when the row is selected
	Bytes   uint64 // reclaimable bytes
	Approx  bool   // render with a leading "~"
	Unknown bool   // not measured; render as "?"
	Files   int    // pinned files behind the row; only a folder has these
	Holders int
	TotalIn int
	Pair    string
	Path    string
}

// CandidateRows flattens a file scan for the picker.
func CandidateRows(cands []Candidate) []pickRow {
	rows := make([]pickRow, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, pickRow{
			ID: c.ID, Bytes: c.Usage.Bytes,
			Approx:  c.Usage.Approx(),
			Unknown: c.Usage.Method == MethodNone,
			Holders: len(c.Copies), TotalIn: c.TotalIn,
			Pair: c.Pair, Path: c.RelPath,
		})
	}
	return rows
}

// FolderRows flattens a folder scan for the picker. The ids are folder ids, so
// what comes back has to be handed to the folder expansion rather than looked
// up as candidates.
func FolderRows(folders []Folder) []pickRow {
	rows := make([]pickRow, 0, len(folders))
	for _, f := range folders {
		path := f.RelPath + "/"
		if f.Kind == FolderThinned {
			// A thinned row does not remove the directory, only files that
			// were deleted out of it, and the list is the one place a reader
			// cannot see the KIND column to tell the difference.
			path = f.RelPath + "/ (thinned)"
		}
		rows = append(rows, pickRow{
			ID: f.ID, Bytes: f.Usage.Bytes,
			Approx:  f.Usage.Approx(),
			Unknown: f.Usage.Method == MethodNone,
			Files:   f.Files,
			Holders: f.Snaps(), TotalIn: f.TotalIn,
			Pair: f.Pair, Path: path,
		})
	}
	return rows
}

// RunPicker shows an interactive checklist and returns the ids the user
// selected. It is a plain raw-mode renderer rather than a full TUI dependency:
// the tool has to stay a single self-contained binary.
//
// title names what is being chosen, because the same widget picks files after
// a plain scan and whole folders after `scan --folders`, and the two are very
// different things to confirm.
func RunPicker(rows []pickRow, title string) ([]int, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("--interactive needs a terminal on stdin")
	}
	if len(rows) == 0 {
		return nil, nil
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("entering raw mode: %w", err)
	}
	restore := func() {
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Fprint(os.Stdout, "\033[?25h") // show cursor
	}
	RegisterCleanup(restore)
	defer restore()
	fmt.Fprint(os.Stdout, "\033[?25l") // hide cursor

	p := &picker{
		rows:     rows,
		title:    title,
		selected: map[int]bool{},
	}
	for _, r := range rows {
		if r.Files > 0 {
			p.showFiles = true
			break
		}
	}
	p.applyFilter()

	buf := make([]byte, 16)
	for {
		p.draw()
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil, err
		}
		done, abort := p.handle(buf[:n])
		if abort {
			p.clear()
			return nil, fmt.Errorf("cancelled")
		}
		if done {
			p.clear()
			return p.result(), nil
		}
	}
}

type picker struct {
	rows      []pickRow
	title     string
	showFiles bool  // the rows carry file counts, so give them a column
	view      []int // indices into rows, after filtering
	selected  map[int]bool
	cursor    int
	offset    int
	filter    string
	filtered  bool
	height    int
}

func (p *picker) applyFilter() {
	p.view = p.view[:0]
	needle := strings.ToLower(p.filter)
	for i, r := range p.rows {
		if needle == "" || strings.Contains(strings.ToLower(r.Path), needle) {
			p.view = append(p.view, i)
		}
	}
	if p.cursor >= len(p.view) {
		p.cursor = max(0, len(p.view)-1)
	}
}

// allShownSelected reports whether every row the filter is showing is already
// ticked, which is what makes "a" a toggle rather than a one-way action.
func (p *picker) allShownSelected() bool {
	if len(p.view) == 0 {
		return false
	}
	for _, i := range p.view {
		if !p.selected[p.rows[i].ID] {
			return false
		}
	}
	return true
}

// setShown ticks or clears every row the filter is showing.
func (p *picker) setShown(on bool) {
	for _, i := range p.view {
		p.selected[p.rows[i].ID] = on
	}
}

// setAll ticks or clears every row, filtered out or not. Without it "select
// all" would quietly mean "all of the ones I happen to be looking at", which
// is the sort of thing that is only noticed after the fact.
func (p *picker) setAll(on bool) {
	for _, r := range p.rows {
		p.selected[r.ID] = on
	}
}

// text renders one row's columns.
func (p *picker) text(r pickRow) string {
	size := FormatBytes(r.Bytes)
	switch {
	case r.Unknown:
		size = "?"
	case r.Approx:
		size = "~" + size
	}
	if p.showFiles {
		return fmt.Sprintf("%10s  %7d files  %d/%d  %-7s %s",
			size, r.Files, r.Holders, r.TotalIn, r.Pair, r.Path)
	}
	return fmt.Sprintf("%10s  %d/%d  %-7s %s", size, r.Holders, r.TotalIn, r.Pair, r.Path)
}

func (p *picker) size() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

func (p *picker) draw() {
	w, h := p.size()
	p.height = max(3, h-6)

	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.height {
		p.offset = p.cursor - p.height + 1
	}

	var b strings.Builder
	b.WriteString("\033[H\033[2J")
	b.WriteString(p.title + "\r\n")
	b.WriteString("\033[2mspace toggle · a all shown · A all · n none · / filter · enter confirm · q cancel\033[0m\r\n\r\n")

	for row := 0; row < p.height; row++ {
		idx := p.offset + row
		if idx >= len(p.view) {
			b.WriteString("\r\n")
			continue
		}
		r := p.rows[p.view[idx]]
		mark := " "
		if p.selected[r.ID] {
			mark = "x"
		}
		cursor := "  "
		if idx == p.cursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s[%s] %s", cursor, mark, p.text(r))
		if len(line) > w-1 {
			line = Truncate(line, w-1)
		}
		if idx == p.cursor {
			b.WriteString("\033[7m" + line + "\033[0m")
		} else {
			b.WriteString(line)
		}
		b.WriteString("\r\n")
	}

	var (
		total uint64
		files int
	)
	for _, r := range p.rows {
		if p.selected[r.ID] {
			total += r.Bytes
			files += r.Files
		}
	}
	b.WriteString("\r\n")
	if p.filtered {
		b.WriteString(fmt.Sprintf("filter: %s\033[7m \033[0m\r\n", p.filter))
	} else {
		b.WriteString(fmt.Sprintf("\033[1m%d of %d selected · %s reclaimable\033[0m",
			p.countSelected(), len(p.rows), FormatBytes(total)))
		if p.showFiles && files > 0 {
			b.WriteString(fmt.Sprintf("\033[1m · %d file(s)\033[0m", files))
		}
		if p.filter != "" {
			b.WriteString(fmt.Sprintf("  \033[2m(filter %q, %d shown)\033[0m", p.filter, len(p.view)))
		}
		b.WriteString("\r\n")
	}
	fmt.Fprint(os.Stdout, b.String())
}

func (p *picker) countSelected() int {
	n := 0
	for _, v := range p.selected {
		if v {
			n++
		}
	}
	return n
}

func (p *picker) clear() {
	fmt.Fprint(os.Stdout, "\033[H\033[2J")
}

// handle processes one input chunk, returning whether to confirm or abort.
func (p *picker) handle(in []byte) (done, abort bool) {
	if p.filtered {
		return p.handleFilter(in)
	}
	switch {
	case len(in) >= 3 && in[0] == 0x1b && in[1] == '[':
		switch in[2] {
		case 'A':
			p.move(-1)
		case 'B':
			p.move(1)
		case '5': // page up
			p.move(-p.height)
		case '6': // page down
			p.move(p.height)
		}
		return false, false
	case len(in) == 1:
		switch in[0] {
		case 'q', 0x03: // q, Ctrl-C
			return false, true
		case '\r', '\n':
			return true, false
		case ' ':
			p.toggle()
		case 'j':
			p.move(1)
		case 'k':
			p.move(-1)
		case 'g':
			p.cursor = 0
		case 'G':
			p.cursor = max(0, len(p.view)-1)
		case 'a':
			// A toggle, so the same key undoes it. Pressing "a" by accident on
			// a list of forty thousand rows should not need "n" and a rebuild
			// of whatever was selected before.
			p.setShown(!p.allShownSelected())
		case 'A':
			p.setAll(true)
		case 'n':
			p.selected = map[int]bool{}
		case '/':
			p.filtered = true
		}
	}
	return false, false
}

func (p *picker) handleFilter(in []byte) (done, abort bool) {
	for _, b := range in {
		switch {
		case b == '\r' || b == '\n' || b == 0x1b:
			p.filtered = false
		case b == 0x03:
			return false, true
		case b == 0x7f || b == 0x08:
			if p.filter != "" {
				p.filter = p.filter[:len(p.filter)-1]
			}
		case b >= 0x20 && b < 0x7f:
			p.filter += string(rune(b))
		}
	}
	p.applyFilter()
	return false, false
}

func (p *picker) move(delta int) {
	if len(p.view) == 0 {
		return
	}
	p.cursor = min(max(p.cursor+delta, 0), len(p.view)-1)
}

func (p *picker) toggle() {
	if p.cursor >= len(p.view) {
		return
	}
	id := p.rows[p.view[p.cursor]].ID
	p.selected[id] = !p.selected[id]
}

func (p *picker) result() []int {
	var out []int
	for id, ok := range p.selected {
		if ok {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}
