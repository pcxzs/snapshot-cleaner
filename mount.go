package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Mounter provides access to a btrfs filesystem's top level (subvolid=5),
// where snapshot managers normally keep snapshots outside any mounted
// subvolume. It reuses an existing top-level mount when one exists and
// otherwise makes a private read-only one, always cleaning up after itself.
type Mounter struct {
	root string

	mu      sync.Mutex
	created map[string]string // device -> mount point we created
	reused  map[string]string // device -> pre-existing mount point
}

func NewMounter(root string) *Mounter {
	return &Mounter{
		root:    root,
		created: map[string]string{},
		reused:  map[string]string{},
	}
}

// TopLevel returns a directory where the top level of the filesystem backing
// device is readable.
func (m *Mounter) TopLevel(device string, mounts []MountEntry) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if p, ok := m.created[device]; ok {
		return p, nil
	}
	if p, ok := m.reused[device]; ok {
		return p, nil
	}

	// A mount whose in-filesystem root is "/" already exposes the top level.
	for _, e := range mounts {
		if e.FSType == "btrfs" && e.Source == device && e.Root == "/" {
			m.reused[device] = e.MountPoint
			Debugf("mount", "reusing existing top-level mount of %s at %s", device, e.MountPoint)
			return e.MountPoint, nil
		}
	}

	if os.Geteuid() != 0 {
		Debugf("mount", "cannot mount top level of %s: not root", device)
		return "", fmt.Errorf("need root to mount the btrfs top level of %s", device)
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return "", err
	}
	target, err := os.MkdirTemp(m.root, "top-")
	if err != nil {
		return "", err
	}

	// Read-only, and with nosuid/nodev/noexec: nothing in a snapshot should be
	// executable or trusted just because we mounted it to look at file sizes.
	const flags = unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	if err := unix.Mount(device, target, "btrfs", flags, "subvolid=5"); err != nil {
		os.Remove(target)
		Errorf("mount", "mounting %s subvolid=5 at %s failed: %v", device, target, err)
		return "", fmt.Errorf("mount %s subvolid=5 at %s: %w", device, target, err)
	}
	m.created[device] = target
	Infof("mount", "mounted %s subvolid=5 read-only at %s", device, target)
	return target, nil
}

// Remount switches a mount we created between read-only and read-write. Purge
// needs write access, and taking it only for that phase keeps the scan path
// incapable of modifying anything.
func (m *Mounter) Remount(mountPoint string, readOnly bool) error {
	m.mu.Lock()
	ours := false
	for _, p := range m.created {
		if p == mountPoint {
			ours = true
			break
		}
	}
	m.mu.Unlock()
	if !ours {
		// A mount we did not create belongs to the system; leave it alone.
		return nil
	}
	flags := uintptr(unix.MS_REMOUNT | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount("", mountPoint, "", flags, ""); err != nil {
		Debugf("mount", "remount of %s to rw=%v failed: %v", mountPoint, !readOnly, err)
		return fmt.Errorf("remount %s rw=%v: %w", mountPoint, !readOnly, err)
	}
	Infof("mount", "remounted %s as %s", mountPoint, map[bool]string{true: "read-only", false: "read-write"}[readOnly])
	return nil
}

// Cleanup unmounts everything this process mounted. Safe to call more than
// once, which matters because it runs from both a deferred call and the signal
// handler.
func (m *Mounter) Cleanup() []error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for device, target := range m.created {
		Debugf("mount", "unmounting %s (%s)", target, device)
		if err := unix.Unmount(target, 0); err != nil {
			// Detach lazily rather than leaving a mount behind on a busy tree.
			Warnf("mount", "plain unmount of %s failed (%v), detaching lazily", target, err)
			if err2 := unix.Unmount(target, unix.MNT_DETACH); err2 != nil {
				Errorf("mount", "lazy detach of %s also failed: %v", target, err2)
				errs = append(errs, fmt.Errorf("unmount %s: %w", target, err))
				continue
			}
		}
		os.Remove(target)
		delete(m.created, device)
	}
	// Remove the mount root if it is now empty.
	if entries, err := os.ReadDir(m.root); err == nil && len(entries) == 0 {
		os.Remove(m.root)
	}
	return errs
}

// MountedPaths lists mount points this process created, for diagnostics.
func (m *Mounter) MountedPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.created {
		out = append(out, filepath.Clean(p))
	}
	return out
}

// OwnedMountFor returns the mount point this process created that contains
// path, or "" when path is reached through a mount we do not own. Purge uses it
// to know which mount needs to be made writable.
func (m *Mounter) OwnedMountFor(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range m.created {
		clean := filepath.Clean(target)
		if path == clean || strings.HasPrefix(path, clean+string(os.PathSeparator)) {
			return clean
		}
	}
	return ""
}
