package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMountInfoLine(t *testing.T) {
	// Real lines from a btrfs system, including the variable-length optional
	// field section that must be skipped by finding the "-" separator.
	line := `35 1 0:31 /@ / rw,noatime shared:1 - btrfs /dev/nvme0n1p2 rw,compress=zstd:1,subvolid=349,subvol=/@`
	e, ok := parseMountInfoLine(line)
	if !ok {
		t.Fatal("failed to parse")
	}
	if e.MountPoint != "/" || e.FSType != "btrfs" || e.Source != "/dev/nvme0n1p2" {
		t.Fatalf("bad parse: %+v", e)
	}
	if e.Root != "/@" || e.Subvol != "/@" || e.SubvolID != 349 {
		t.Fatalf("bad btrfs fields: root=%q subvol=%q subvolid=%d", e.Root, e.Subvol, e.SubvolID)
	}
}

func TestParseMountInfoLineNoOptionalFields(t *testing.T) {
	line := `26 33 0:6 / /dev rw,nosuid,relatime - devtmpfs udev rw,size=16146116k`
	e, ok := parseMountInfoLine(line)
	if !ok {
		t.Fatal("failed to parse")
	}
	if e.MountPoint != "/dev" || e.FSType != "devtmpfs" {
		t.Fatalf("bad parse: %+v", e)
	}
}

func TestParseMountInfoLineEscapedPath(t *testing.T) {
	line := `40 35 0:31 /@home /mnt/my\040disk rw - btrfs /dev/sda1 rw,subvol=/@home`
	e, ok := parseMountInfoLine(line)
	if !ok {
		t.Fatal("failed to parse")
	}
	if e.MountPoint != "/mnt/my disk" {
		t.Fatalf("octal escape not decoded: %q", e.MountPoint)
	}
}

func TestParseMountInfoLineRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not a mount line", "1 2 3"} {
		if _, ok := parseMountInfoLine(bad); ok {
			t.Errorf("should have rejected %q", bad)
		}
	}
}

func TestFindMountPicksLongestPrefix(t *testing.T) {
	mounts := []MountEntry{
		{MountPoint: "/", FSType: "btrfs"},
		{MountPoint: "/home", FSType: "btrfs"},
		{MountPoint: "/home/user/private", FSType: "btrfs"},
	}
	// A path under a nested mount must resolve to that mount, not its parent.
	if m := FindMount(mounts, "/home/user/private/secret"); m == nil || m.MountPoint != "/home/user/private" {
		t.Fatalf("got %+v", m)
	}
	if m := FindMount(mounts, "/home/user/file"); m == nil || m.MountPoint != "/home" {
		t.Fatalf("got %+v", m)
	}
	if m := FindMount(mounts, "/etc/fstab"); m == nil || m.MountPoint != "/" {
		t.Fatalf("got %+v", m)
	}
	// "/homework" must not match the "/home" mount.
	if m := FindMount(mounts, "/homework/file"); m == nil || m.MountPoint != "/" {
		t.Fatalf("prefix match leaked across a path component: %+v", m)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader(`
# a comment
min-size = 100M
cache-min-size = 4M
provider = snapper

pair = /home : /mnt/btr/snapshots/home
pair = / : /mnt/btr/snapshots/root   # trailing comment
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinSize != "100M" || cfg.Provider != "snapper" || cfg.CacheMinSize != "4M" {
		t.Fatalf("bad scalars: %+v", cfg)
	}
	if len(cfg.Pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d", len(cfg.Pairs))
	}
	if cfg.Pairs[0].Live != "/home" || cfg.Pairs[0].Snapshots != "/mnt/btr/snapshots/home" {
		t.Fatalf("bad pair: %+v", cfg.Pairs[0])
	}
	if cfg.Pairs[1].Live != "/" {
		t.Fatalf("bad pair: %+v", cfg.Pairs[1])
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	for _, bad := range []string{
		"nonsense-key = 1",
		"pair = /home", // missing the ":" separator
		"min-size = not-a-size",
		"cache-min-size = not-a-size",
		"no equals sign here",
	} {
		if _, err := parseConfig(strings.NewReader(bad)); err == nil {
			t.Errorf("should have rejected %q", bad)
		}
	}
}

func TestParseTimeshiftName(t *testing.T) {
	got := parseTimeshiftName("2026-09-03_21-00-01")
	if got.IsZero() {
		t.Fatal("failed to parse a valid Timeshift directory name")
	}
	if got.Year() != 2026 || got.Month() != time.September || got.Day() != 3 || got.Hour() != 21 {
		t.Fatalf("bad time: %v", got)
	}
	if !parseTimeshiftName("not-a-date").IsZero() {
		t.Error("should return zero time for a non-date name")
	}
}

func TestReadTimeshiftInfo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "info.json"), []byte(`{
  "created" : "2026-09-01 11:00:00",
  "tags" : "W",
  "comments" : "weekly"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	created, tag := readTimeshiftInfo(dir, "2026-09-03_21-00-01")
	if tag != "weekly" {
		t.Errorf("tag = %q, want weekly", tag)
	}
	// info.json wins over the directory name.
	if created.Day() != 1 || created.Hour() != 11 {
		t.Errorf("created = %v, want the info.json value", created)
	}
}

func TestReadTimeshiftInfoFallsBackToDirName(t *testing.T) {
	dir := t.TempDir() // no info.json at all
	created, _ := readTimeshiftInfo(dir, "2026-09-03_21-00-01")
	if created.Day() != 3 || created.Hour() != 21 {
		t.Errorf("created = %v, want the directory-name value", created)
	}
}

func TestReadSnapperInfo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "info.xml"), []byte(`<?xml version="1.0"?>
<snapshot>
  <type>single</type>
  <num>42</num>
  <date>2026-09-03 18:30:00</date>
  <description>timeline</description>
  <cleanup>timeline</cleanup>
</snapshot>`), 0o600); err != nil {
		t.Fatal(err)
	}
	created, tag := readSnapperInfo(dir)
	if tag != "timeline" {
		t.Errorf("tag = %q", tag)
	}
	if created.IsZero() || created.Day() != 3 {
		t.Errorf("created = %v", created)
	}
}

func TestBtrbkSnapshotNaming(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantSubvol string
		wantOK     bool
	}{
		{"home.20260903", "home", true},
		{"home.20260903T2100", "home", true},
		{"home.20260903T210000", "home", true},
		{"home.20260903_1", "home", true},
		{"data.root.20260903", "data.root", true},
		{"not-a-snapshot", "", false},
		{"home.2026", "", false},
	} {
		m := btrbkSnapshotRE.FindStringSubmatch(tc.name)
		if (m != nil) != tc.wantOK {
			t.Errorf("%q: match=%v, want %v", tc.name, m != nil, tc.wantOK)
			continue
		}
		if tc.wantOK && m[1] != tc.wantSubvol {
			t.Errorf("%q: subvolume = %q, want %q", tc.name, m[1], tc.wantSubvol)
		}
	}
}

func TestParseBtrbkTime(t *testing.T) {
	for _, s := range []string{"20260903", "20260903T2100", "20260903T210000"} {
		if got := parseBtrbkTime(s); got.IsZero() || got.Year() != 2026 {
			t.Errorf("parseBtrbkTime(%q) = %v", s, got)
		}
	}
	if !parseBtrbkTime("garbage").IsZero() {
		t.Error("should return zero time for garbage")
	}
}

func TestExcluded(t *testing.T) {
	pats := []string{"*.log", "cache/*"}
	for _, tc := range []struct {
		rel, base string
		want      bool
	}{
		{"var/app.log", "app.log", true},
		{"cache/big.bin", "big.bin", true},
		{"docs/report.pdf", "report.pdf", false},
	} {
		if got := excluded(tc.rel, tc.base, pats); got != tc.want {
			t.Errorf("excluded(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
	if excluded("anything", "anything", nil) {
		t.Error("no patterns should exclude nothing")
	}
}
