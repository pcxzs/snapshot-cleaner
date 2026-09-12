package main

import (
	"path/filepath"
	"testing"
)

// A mount under the tool's own mount root is one of ours whichever run made it.
// The one that matters is a leftover from a run that was killed before it could
// clean up: inherited as the system's, it is read-only forever, and a purge
// through it fails on every single file.
func TestTopLevelAdoptsAStaleMountOfItsOwn(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "top-12345")
	m := NewMounter(root)

	got, err := m.TopLevel("/dev/vda1", []MountEntry{
		{FSType: "btrfs", Source: "/dev/vda1", Root: "/", MountPoint: stale},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != stale {
		t.Fatalf("TopLevel = %q, want the existing mount %q", got, stale)
	}
	if owned := m.OwnedMountFor(filepath.Join(stale, "snapshots/2026-05-25/@/etc/hosts")); owned != stale {
		t.Errorf("OwnedMountFor = %q, want %q: a stale mount of our own must be ours to remount",
			owned, stale)
	}
	if paths := m.MountedPaths(); len(paths) != 1 || paths[0] != stale {
		t.Errorf("MountedPaths = %v, want the adopted mount so cleanup unmounts it", paths)
	}
}

// Someone else's mount of the same filesystem is still reused for reading, but
// never remounted or unmounted by us.
func TestTopLevelLeavesASystemMountAlone(t *testing.T) {
	m := NewMounter(t.TempDir())
	const system = "/mnt/btrfs-root"

	got, err := m.TopLevel("/dev/vda1", []MountEntry{
		{FSType: "btrfs", Source: "/dev/vda1", Root: "/", MountPoint: system},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != system {
		t.Fatalf("TopLevel = %q, want %q", got, system)
	}
	if owned := m.OwnedMountFor(system + "/snapshots/2026-05-25/@/etc/hosts"); owned != "" {
		t.Errorf("OwnedMountFor = %q, want \"\": a system mount is not ours to remount", owned)
	}
	if paths := m.MountedPaths(); len(paths) != 0 {
		t.Errorf("MountedPaths = %v, want none: we did not mount it", paths)
	}
}

// A plan can span two filesystems, each with its own top-level mount. Making
// only the first writable would leave the second failing every unlink.
func TestOwnedMountsForCoversEveryFilesystem(t *testing.T) {
	root := t.TempDir()
	m := NewMounter(root)
	var mounts []string
	for _, dev := range []string{"/dev/sda1", "/dev/sdb1"} {
		mp := filepath.Join(root, "top-"+filepath.Base(dev))
		if _, err := m.TopLevel(dev, []MountEntry{
			{FSType: "btrfs", Source: dev, Root: "/", MountPoint: mp},
		}); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, mp)
	}

	got := m.OwnedMountsFor([]string{
		filepath.Join(mounts[0], "snap/a/file"),
		filepath.Join(mounts[0], "snap/b/file"), // same mount, listed once
		filepath.Join(mounts[1], "snap/c/file"),
		"/somewhere/else/entirely",
	})
	if len(got) != 2 || got[0] != mounts[0] || got[1] != mounts[1] {
		t.Errorf("OwnedMountsFor = %v, want %v", got, mounts)
	}
}

// The preflight passes on an ordinary writable filesystem, and names a mount
// that can be acted on.
func TestRequireWritableSnapshotsAcceptsAWritableTree(t *testing.T) {
	dir := t.TempDir()
	plan := &PurgePlan{Targets: []PurgeTarget{{Copy: Copy{Snapshot: dir}}}}
	if err := requireWritableSnapshots(plan); err != nil {
		t.Errorf("requireWritableSnapshots on a writable tree: %v", err)
	}
	// Whatever the layout - a dedicated /tmp, or everything on the root mount,
	// which is what CI runs on - the path is under some mount and it must be
	// named, or the error this feeds says nothing the user can act on.
	mp := mountPointOf(dir)
	if mp == "" {
		t.Fatalf("mountPointOf(%s) = \"\", want the mount it is reached through", dir)
	}
	if !under(dir, mp) {
		t.Errorf("mountPointOf(%s) = %q, which does not contain it", dir, mp)
	}
}

// Every path is under the root mount, and "/" is the one mount point that a
// naive prefix join gets wrong.
func TestUnderHandlesTheRootMount(t *testing.T) {
	cases := []struct {
		path, mount string
		want        bool
	}{
		{"/tmp/x/y", "/", true},
		{"/tmp/x/y", "/tmp", true},
		{"/tmp/x/y", "/tmp/x/y", true},
		{"/tmp/xy", "/tmp/x", false},
		{"/var/tmp", "/tmp", false},
	}
	for _, c := range cases {
		if got := under(c.path, c.mount); got != c.want {
			t.Errorf("under(%q, %q) = %v, want %v", c.path, c.mount, got, c.want)
		}
	}
}
