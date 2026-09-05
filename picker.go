package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// RunPicker shows an interactive checklist of candidates and returns the ids
// the user selected. It is a plain raw-mode renderer rather than a full TUI
// dependency: the tool has to stay a single self-contained binary.
func RunPicker(cands []Candidate) ([]int, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("--interactive needs a terminal on stdin")
	}
	if len(cands) == 0 {
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
		cands:    cands,
		selected: map[int]bool{},
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
	cands    []Candidate
	view     []int // indices into cands, after filtering
	selected map[int]bool
	cursor   int
	offset   int
	filter   string
	filtered bool
	rows     int
}

func (p *picker) applyFilter() {
	p.view = p.view[:0]
	needle := strings.ToLower(p.filter)
	for i, c := range p.cands {
		if needle == "" || strings.Contains(strings.ToLower(c.RelPath), needle) {
			p.view = append(p.view, i)
		}
	}
	if p.cursor >= len(p.view) {
		p.cursor = max(0, len(p.view)-1)
	}
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
	p.rows = max(3, h-6)

	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.rows {
		p.offset = p.cursor - p.rows + 1
	}

	var b strings.Builder
	b.WriteString("\033[H\033[2J")
	b.WriteString("Select files to remove from their snapshots\r\n")
	b.WriteString("\033[2mspace toggle · a all · n none · / filter · enter confirm · q cancel\033[0m\r\n\r\n")

	for row := 0; row < p.rows; row++ {
		idx := p.offset + row
		if idx >= len(p.view) {
			b.WriteString("\r\n")
			continue
		}
		c := p.cands[p.view[idx]]
		mark := " "
		if p.selected[c.ID] {
			mark = "x"
		}
		cursor := "  "
		if idx == p.cursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s[%s] %9s  %d/%d  %-7s %s",
			cursor, mark, FormatBytes(c.Usage.Bytes), len(c.Copies), c.TotalIn, c.Pair, c.RelPath)
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

	var total uint64
	for _, c := range p.cands {
		if p.selected[c.ID] {
			total += c.Usage.Bytes
		}
	}
	b.WriteString("\r\n")
	if p.filtered {
		b.WriteString(fmt.Sprintf("filter: %s\033[7m \033[0m\r\n", p.filter))
	} else {
		b.WriteString(fmt.Sprintf("\033[1m%d selected · %s reclaimable\033[0m", p.countSelected(), FormatBytes(total)))
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
			p.move(-p.rows)
		case '6': // page down
			p.move(p.rows)
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
			for _, i := range p.view {
				p.selected[p.cands[i].ID] = true
			}
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
	id := p.cands[p.view[p.cursor]].ID
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
