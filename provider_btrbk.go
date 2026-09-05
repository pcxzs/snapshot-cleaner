package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// BtrbkProvider handles btrbk, which stores snapshots as
// <snapshot_dir>/<name>.<timestamp> subvolumes.
type BtrbkProvider struct{}

func (*BtrbkProvider) Name() string { return "btrbk" }

var btrbkConfigPaths = []string{"/etc/btrbk/btrbk.conf", "/etc/btrbk.conf"}

// btrbkSnapshotRE matches btrbk's default naming: name.YYYYMMDD with an
// optional THHMM time part and an optional numeric suffix for same-day repeats.
var btrbkSnapshotRE = regexp.MustCompile(`^(.+)\.(\d{8}(?:T\d{4,6})?)(?:_(\d+))?$`)

// btrbkTarget is one live subvolume together with the directory holding its
// snapshots.
type btrbkTarget struct {
	live      string
	snapshots string
	name      string
}

func (p *BtrbkProvider) Detect(sys *System) ([]Pair, error) {
	targets := parseBtrbkConfigs(sys)
	if len(targets) == 0 {
		return nil, nil
	}

	var pairs []Pair
	for _, t := range targets {
		entries, err := os.ReadDir(t.snapshots)
		if err != nil {
			sys.Notef("btrbk: cannot read snapshot dir %s: %v", t.snapshots, err)
			continue
		}
		pair := Pair{Provider: p.Name(), Name: t.name, Live: t.live}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			m := btrbkSnapshotRE.FindStringSubmatch(e.Name())
			if m == nil || m[1] != t.name {
				continue
			}
			root := filepath.Join(t.snapshots, e.Name())
			if !IsSubvolRoot(root) {
				continue
			}
			pair.Snapshots = append(pair.Snapshots, describeSnapshot(e.Name(), root, parseBtrbkTime(m[2]), ""))
		}
		if len(pair.Snapshots) == 0 {
			continue
		}
		sortSnapshots(pair.Snapshots)
		pairs = append(pairs, pair)
	}
	return pairs, nil
}

func parseBtrbkTime(s string) time.Time {
	for _, layout := range []string{"20060102T150405", "20060102T1504", "20060102"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseBtrbkConfigs walks btrbk's indentation-scoped config, tracking the
// enclosing volume and subvolume so relative snapshot_dir values resolve
// correctly.
func parseBtrbkConfigs(sys *System) []btrbkTarget {
	var out []btrbkTarget
	for _, path := range btrbkConfigPaths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}

		var volume, subvolume, globalSnapDir, volumeSnapDir, subvolSnapDir string
		flush := func() {
			if volume == "" || subvolume == "" {
				return
			}
			dir := subvolSnapDir
			if dir == "" {
				dir = volumeSnapDir
			}
			if dir == "" {
				dir = globalSnapDir
			}
			if dir == "" {
				// btrbk's default is to snapshot beside the subvolume.
				dir = volume
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(volume, dir)
			}
			out = append(out, btrbkTarget{
				live:      filepath.Join(volume, subvolume),
				snapshots: filepath.Clean(dir),
				name:      filepath.Base(subvolume),
			})
		}

		sc := bufio.NewScanner(f)
		for sc.Scan() {
			raw := sc.Text()
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, _ := strings.Cut(line, " ")
			value = strings.TrimSpace(value)

			switch key {
			case "volume":
				flush()
				volume, subvolume = value, ""
				volumeSnapDir, subvolSnapDir = "", ""
			case "subvolume":
				flush()
				subvolume = value
				subvolSnapDir = ""
			case "snapshot_dir":
				switch {
				case subvolume != "":
					subvolSnapDir = value
				case volume != "":
					volumeSnapDir = value
				default:
					globalSnapDir = value
				}
			}
		}
		flush()
		f.Close()
	}

	if len(out) == 0 {
		sys.Notef("btrbk: no config found at %s", strings.Join(btrbkConfigPaths, " or "))
	}
	return out
}
