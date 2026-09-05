package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Lock is an advisory exclusive lock preventing two runs from working on the
// same snapshots at once.
type Lock struct{ f *os.File }

// AcquireLock takes the lock without blocking. A second instance should fail
// fast and say so rather than queue up behind a long scan.
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		Errorf("lock", "could not acquire %s: %v", path, err)
		return nil, fmt.Errorf("another %s is already running (lock: %s)", appName, path)
	}
	Debugf("lock", "acquired %s", path)
	return &Lock{f: f}, nil
}

func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
}

// snapshotManagers are the processes that create and rotate snapshots. Running
// while one of them is mid-operation risks acting on a snapshot that is being
// created or deleted underneath us.
var snapshotManagers = []string{"timeshift", "timeshift-gtk", "snapper", "btrbk"}

// RunningSnapshotManagers returns the names of any snapshot manager currently
// running.
func RunningSnapshotManagers() []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(data))
		for _, m := range snapshotManagers {
			if comm == m && !seen[comm] {
				seen[comm] = true
				out = append(out, comm)
			}
		}
	}
	return out
}
