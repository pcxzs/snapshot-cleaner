package main

import (
	"bufio"
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SnapperProvider handles snapper, whose snapshots live at
// <subvolume>/.snapshots/<N>/snapshot with metadata in <N>/info.xml.
type SnapperProvider struct{}

func (*SnapperProvider) Name() string { return "snapper" }

const snapperConfigDir = "/etc/snapper/configs"

type snapperInfo struct {
	XMLName     xml.Name `xml:"snapshot"`
	Type        string   `xml:"type"`
	Num         int      `xml:"num"`
	Date        string   `xml:"date"`
	Description string   `xml:"description"`
	Cleanup     string   `xml:"cleanup"`
}

func (p *SnapperProvider) Detect(sys *System) ([]Pair, error) {
	// Configured subvolumes are authoritative; scanning mounts catches setups
	// where the configs are absent or unreadable.
	candidates := snapperConfiguredSubvolumes(sys)
	for _, m := range BtrfsMounts(sys.Mounts) {
		candidates = append(candidates, m.MountPoint)
	}

	seen := map[string]bool{}
	var pairs []Pair
	for _, live := range candidates {
		live = filepath.Clean(live)
		if seen[live] {
			continue
		}
		seen[live] = true

		snapsDir := filepath.Join(live, ".snapshots")
		entries, err := os.ReadDir(snapsDir)
		if err != nil {
			Tracef("snapper", "no .snapshots under %s: %v", live, err)
			continue
		}
		Debugf("snapper", "found %d entr(ies) under %s", len(entries), snapsDir)

		pair := Pair{Provider: p.Name(), Name: snapperPairName(live), Live: live}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			// snapper numbers its snapshots; anything else is not one.
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			root := filepath.Join(snapsDir, e.Name(), "snapshot")
			if !IsSubvolRoot(root) {
				continue
			}
			created, tag := readSnapperInfo(filepath.Join(snapsDir, e.Name()))
			pair.Snapshots = append(pair.Snapshots, describeSnapshot(e.Name(), root, created, tag))
		}
		if len(pair.Snapshots) == 0 {
			continue
		}
		sortSnapshots(pair.Snapshots)
		pairs = append(pairs, pair)
	}
	return pairs, nil
}

// snapperPairName gives the pair a readable label: the mount point itself,
// except for "/" where a bare slash reads poorly in a table.
func snapperPairName(live string) string {
	if live == "/" {
		return "root"
	}
	return strings.TrimPrefix(live, "/")
}

// snapperConfiguredSubvolumes reads SUBVOLUME= out of the snapper configs.
func snapperConfiguredSubvolumes(sys *System) []string {
	entries, err := os.ReadDir(snapperConfigDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(snapperConfigDir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(line, "SUBVOLUME="); ok {
				out = append(out, strings.Trim(strings.TrimSpace(v), `"`))
			}
		}
		f.Close()
	}
	if len(out) == 0 {
		sys.Notef("snapper: %s exists but declares no SUBVOLUME", snapperConfigDir)
	}
	sort.Strings(out)
	return out
}

func readSnapperInfo(dir string) (time.Time, string) {
	data, err := os.ReadFile(filepath.Join(dir, "info.xml"))
	if err != nil {
		return time.Time{}, ""
	}
	var info snapperInfo
	if err := xml.Unmarshal(data, &info); err != nil {
		return time.Time{}, ""
	}
	// snapper writes UTC without a zone marker.
	created, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(info.Date), time.UTC)
	if err != nil {
		created = time.Time{}
	}
	tag := info.Cleanup
	if tag == "" {
		tag = info.Type
	}
	return created, tag
}
