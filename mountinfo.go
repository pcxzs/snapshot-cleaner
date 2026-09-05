package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// MountEntry is one line of /proc/self/mountinfo.
type MountEntry struct {
	ID           int
	ParentID     int
	Major, Minor int
	Root         string // path of the mounted tree within the filesystem
	MountPoint   string
	Options      string
	FSType       string
	Source       string // device
	SuperOptions string
	Subvol       string // btrfs subvol= if present
	SubvolID     uint64 // btrfs subvolid= if present
}

// ReadMounts parses /proc/self/mountinfo.
func ReadMounts() ([]MountEntry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountInfo(f)
}

func parseMountInfo(r io.Reader) ([]MountEntry, error) {
	var out []MountEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		e, ok := parseMountInfoLine(sc.Text())
		if ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// parseMountInfoLine handles the variable-length optional-field section, which
// is terminated by a lone "-" separator.
func parseMountInfoLine(line string) (MountEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 7 {
		return MountEntry{}, false
	}
	sep := -1
	for i, f := range fields {
		if f == "-" {
			sep = i
			break
		}
	}
	if sep < 6 || sep+3 > len(fields) {
		return MountEntry{}, false
	}

	var e MountEntry
	e.ID, _ = strconv.Atoi(fields[0])
	e.ParentID, _ = strconv.Atoi(fields[1])
	if maj, min, ok := strings.Cut(fields[2], ":"); ok {
		e.Major, _ = strconv.Atoi(maj)
		e.Minor, _ = strconv.Atoi(min)
	}
	e.Root = unescapeOctal(fields[3])
	e.MountPoint = unescapeOctal(fields[4])
	e.Options = fields[5]
	e.FSType = fields[sep+1]
	e.Source = unescapeOctal(fields[sep+2])
	if sep+3 < len(fields) {
		e.SuperOptions = fields[sep+3]
	}

	for _, opt := range strings.Split(e.SuperOptions, ",") {
		if v, ok := strings.CutPrefix(opt, "subvol="); ok {
			e.Subvol = v
		}
		if v, ok := strings.CutPrefix(opt, "subvolid="); ok {
			e.SubvolID, _ = strconv.ParseUint(v, 10, 64)
		}
	}
	return e, true
}

// unescapeOctal decodes the \040 style escapes the kernel uses for spaces,
// tabs, newlines and backslashes in mount paths.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// FindMount returns the mount entry whose mount point is the longest prefix of
// path, i.e. the filesystem that actually serves it.
func FindMount(mounts []MountEntry, path string) *MountEntry {
	best := -1
	bestLen := -1
	for i, m := range mounts {
		if m.MountPoint == path || strings.HasPrefix(path, strings.TrimSuffix(m.MountPoint, "/")+"/") {
			if len(m.MountPoint) > bestLen {
				best, bestLen = i, len(m.MountPoint)
			}
		}
	}
	if best < 0 {
		return nil
	}
	return &mounts[best]
}

// BtrfsMounts returns the btrfs mounts, deepest mount point first, so that
// nested subvolume mounts are considered before their parents.
func BtrfsMounts(mounts []MountEntry) []MountEntry {
	var out []MountEntry
	for _, m := range mounts {
		if m.FSType == "btrfs" {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i].MountPoint) > len(out[j].MountPoint)
	})
	return out
}

// DeviceForPath returns the backing device of the filesystem serving path.
func DeviceForPath(path string) (string, error) {
	mounts, err := ReadMounts()
	if err != nil {
		return "", err
	}
	m := FindMount(mounts, path)
	if m == nil {
		return "", fmt.Errorf("no mount found for %s", path)
	}
	return m.Source, nil
}
