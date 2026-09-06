package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FormatBytes renders a byte count in IEC units with a stable width-friendly form.
func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}[exp]
	switch {
	case val >= 100:
		return fmt.Sprintf("%.0f %s", val, suffix)
	case val >= 10:
		return fmt.Sprintf("%.1f %s", val, suffix)
	default:
		return fmt.Sprintf("%.2f %s", val, suffix)
	}
}

// ParseSize accepts forms like "50M", "1.5G", "1024", "10MiB", "2GB".
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	numPart, unitPart := s[:i], strings.ToUpper(strings.TrimSpace(s[i:]))
	if numPart == "" {
		return 0, fmt.Errorf("no number in size %q", s)
	}
	num, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q: %w", s, err)
	}
	if num < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	// Both "M" and "MiB" mean 1024-based; "MB" means 1000-based.
	var mult float64 = 1
	base1000 := strings.HasSuffix(unitPart, "B") && !strings.HasSuffix(unitPart, "IB")
	letter := ""
	if unitPart != "" {
		letter = unitPart[:1]
	}
	base := 1024.0
	if base1000 && len(unitPart) == 2 {
		base = 1000.0
	}
	switch letter {
	case "", "B":
		mult = 1
	case "K":
		mult = base
	case "M":
		mult = base * base
	case "G":
		mult = base * base * base
	case "T":
		mult = base * base * base * base
	case "P":
		mult = base * base * base * base * base
	default:
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return uint64(num * mult), nil
}

// Selection is a parsed list of row ids. A folder scan and a file scan both
// number their rows from 1, so folder rows carry an "F" prefix on the command
// line and the two can never be confused for one another.
type Selection struct {
	Files   []int
	Folders []int
}

// ParseSelection expands selections like "1,3,7-9" and "F2,F5-F7" into sorted,
// de-duplicated lists. In a range the prefix may be written on either end or
// on both, so "F5-F7", "F5-7" and "f5-f7" all mean the same three folders.
func ParseSelection(args []string) (Selection, error) {
	files := map[int]bool{}
	folders := map[int]bool{}
	for _, arg := range args {
		for _, part := range strings.Split(arg, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			token, isFolder := stripFolderPrefix(part)
			seen := files
			if isFolder {
				seen = folders
			}

			lo, hi, isRange := strings.Cut(token, "-")
			if !isRange {
				n, err := strconv.Atoi(token)
				if err != nil {
					return Selection{}, fmt.Errorf("bad id %q", part)
				}
				seen[n] = true
				continue
			}
			hi = strings.TrimSpace(hi)
			if trimmed, hiFolder := stripFolderPrefix(hi); hiFolder {
				// A range spans one view or the other. "3-F5" asks for files
				// and folders at once, which names nothing.
				if !isFolder {
					return Selection{}, fmt.Errorf("range %q mixes a file id and a folder id", part)
				}
				hi = trimmed
			}
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return Selection{}, fmt.Errorf("bad range start in %q", part)
			}
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return Selection{}, fmt.Errorf("bad range end in %q", part)
			}
			if a > b {
				return Selection{}, fmt.Errorf("reversed range %q", part)
			}
			for n := a; n <= b; n++ {
				seen[n] = true
			}
		}
	}
	sel := Selection{Files: sortedKeys(files), Folders: sortedKeys(folders)}
	if len(sel.Files) == 0 && len(sel.Folders) == 0 {
		return Selection{}, fmt.Errorf("no ids given")
	}
	return sel, nil
}

// stripFolderPrefix removes the "F" that marks a folder row, reporting whether
// there was one. A bare "F" is not an id, so it is left to fail as one.
func stripFolderPrefix(s string) (string, bool) {
	if len(s) > 1 && (s[0] == 'F' || s[0] == 'f') {
		return s[1:], true
	}
	return s, false
}

func sortedKeys(m map[int]bool) []int {
	if len(m) == 0 {
		return nil
	}
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// ParseIDs expands selections like "1,3,7-9" into a sorted, de-duplicated list
// of file ids, rejecting folder ids.
func ParseIDs(args []string) ([]int, error) {
	sel, err := ParseSelection(args)
	if err != nil {
		return nil, err
	}
	if len(sel.Folders) > 0 {
		return nil, fmt.Errorf("folder ids are not valid here")
	}
	return sel.Files, nil
}

// Truncate shortens a path for table display, keeping the tail which is the
// informative end of a filesystem path.
func Truncate(s string, max int) string {
	if max <= 3 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return "..." + string(r[len(r)-(max-3):])
}
