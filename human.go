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

// ParseIDs expands selections like "1,3,7-9" into a sorted, de-duplicated list.
func ParseIDs(args []string) ([]int, error) {
	seen := map[int]bool{}
	for _, arg := range args {
		for _, part := range strings.Split(arg, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			lo, hi, isRange := strings.Cut(part, "-")
			if !isRange {
				n, err := strconv.Atoi(part)
				if err != nil {
					return nil, fmt.Errorf("bad id %q", part)
				}
				seen[n] = true
				continue
			}
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return nil, fmt.Errorf("bad range start in %q", part)
			}
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("bad range end in %q", part)
			}
			if a > b {
				return nil, fmt.Errorf("reversed range %q", part)
			}
			for n := a; n <= b; n++ {
				seen[n] = true
			}
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no ids given")
	}
	return out, nil
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
