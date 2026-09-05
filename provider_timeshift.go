package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TimeshiftProvider handles Timeshift in btrfs mode, whose snapshots live at
// <top level>/timeshift-btrfs/snapshots/<name>/<subvolume>.
type TimeshiftProvider struct{}

func (*TimeshiftProvider) Name() string { return "timeshift" }

const timeshiftSnapshotsRel = "timeshift-btrfs/snapshots"

// timeshiftInfo is the subset of Timeshift's info.json we use.
type timeshiftInfo struct {
	Created string `json:"created"`
	Tags    string `json:"tags"`
	Comment string `json:"comments"`
}

// Timeshift tag letters, as written into info.json.
var timeshiftTagNames = map[string]string{
	"B": "boot", "H": "hourly", "D": "daily", "W": "weekly", "M": "monthly", "O": "ondemand",
}

func (p *TimeshiftProvider) Detect(sys *System) ([]Pair, error) {
	// Timeshift also has an rsync mode, which copies files instead of taking
	// btrfs snapshots. Nothing in this tool applies there, so say so plainly
	// rather than silently finding nothing.
	if data, err := os.ReadFile("/etc/timeshift/timeshift.json"); err == nil {
		if !strings.Contains(string(data), `"btrfs_mode" : "true"`) &&
			!strings.Contains(string(data), `"btrfs_mode":"true"`) {
			sys.Notef("timeshift: configured in rsync mode, not btrfs mode - snapshots are file copies, not reflinks")
			return nil, nil
		}
	}

	seenDevice := map[string]bool{}
	var pairs []Pair

	for _, m := range BtrfsMounts(sys.Mounts) {
		if seenDevice[m.Source] {
			continue
		}
		seenDevice[m.Source] = true

		top, err := sys.TopLevelFor(m)
		if err != nil {
			Debugf("timeshift", "cannot reach top level of %s: %v", m.Source, err)
			sys.Notef("timeshift: cannot reach top level of %s: %v", m.Source, err)
			continue
		}
		snapsDir := filepath.Join(top, timeshiftSnapshotsRel)
		entries, err := os.ReadDir(snapsDir)
		if err != nil {
			Debugf("timeshift", "no snapshot directory at %s: %v", snapsDir, err)
			continue
		}
		Infof("timeshift", "found %d entr(ies) under %s", len(entries), snapsDir)

		// subvolume name -> pair, built as we discover which subvolumes the
		// snapshots actually contain. Nothing here assumes "@" or "@home".
		bySubvol := map[string]*Pair{}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			snapDir := filepath.Join(snapsDir, e.Name())
			created, tag := readTimeshiftInfo(snapDir, e.Name())

			inner, err := os.ReadDir(snapDir)
			if err != nil {
				continue
			}
			for _, sub := range inner {
				if !sub.IsDir() {
					continue
				}
				root := filepath.Join(snapDir, sub.Name())
				if !IsSubvolRoot(root) {
					continue
				}
				live := liveMountForSubvol(sys, m.Source, sub.Name())
				if live == "" {
					Warnf("timeshift", "snapshot subvolume %q (in %s) maps to no live mount; skipping",
						sub.Name(), e.Name())
					sys.Notef("timeshift: snapshot subvolume %q is not mounted anywhere; skipping", sub.Name())
					continue
				}
				Tracef("timeshift", "snapshot %s subvol %s -> live %s", e.Name(), sub.Name(), live)
				pr, ok := bySubvol[sub.Name()]
				if !ok {
					pr = &Pair{Provider: p.Name(), Name: sub.Name(), Live: live}
					bySubvol[sub.Name()] = pr
				}
				pr.Snapshots = append(pr.Snapshots, describeSnapshot(e.Name(), root, created, tag))
			}
		}

		for _, pr := range bySubvol {
			sortSnapshots(pr.Snapshots)
			pairs = append(pairs, *pr)
		}
	}
	return pairs, nil
}

// liveMountForSubvol finds where a named subvolume is currently mounted. The
// mountinfo "root" field holds the subvolume path within the filesystem, so
// this maps a snapshot's subvolume back to its live counterpart without
// hardcoding any naming convention.
func liveMountForSubvol(sys *System, device, subvol string) string {
	want := "/" + strings.TrimPrefix(subvol, "/")
	for _, e := range sys.Mounts {
		if e.FSType != "btrfs" || e.Source != device {
			continue
		}
		if e.Root == want || e.Subvol == want || e.Subvol == strings.TrimPrefix(want, "/") {
			return e.MountPoint
		}
	}
	return ""
}

func readTimeshiftInfo(snapDir, dirName string) (time.Time, string) {
	created := parseTimeshiftName(dirName)
	tag := ""

	data, err := os.ReadFile(filepath.Join(snapDir, "info.json"))
	if err != nil {
		return created, tag
	}
	var info timeshiftInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return created, tag
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(info.Created), time.Local); err == nil {
		created = t
	}
	var names []string
	for _, letter := range strings.Fields(info.Tags) {
		if name, ok := timeshiftTagNames[strings.ToUpper(letter)]; ok {
			names = append(names, name)
		}
	}
	return created, strings.Join(names, ",")
}

// parseTimeshiftName reads the timestamp Timeshift encodes in the directory
// name, used when info.json is missing or unreadable.
func parseTimeshiftName(name string) time.Time {
	t, err := time.ParseInLocation("2006-01-02_15-04-05", name, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}
