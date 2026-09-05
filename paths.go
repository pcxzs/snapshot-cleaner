package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const appName = "snapshot-cleaner"

// Dirs holds the resolved locations the tool writes to. Every location is
// probed for writability and falls back, so the tool works on systems without
// systemd, without /var/lib, or when run from a read-only root.
type Dirs struct {
	State   string // scan state and deletion journal
	Runtime string // lock file and temporary mount points
}

func firstWritableDir(candidates []string) (string, error) {
	var lastErr error
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if err := os.MkdirAll(c, 0o700); err != nil {
			lastErr = err
			continue
		}
		probe := filepath.Join(c, ".write-probe")
		f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			lastErr = err
			continue
		}
		f.Close()
		os.Remove(probe)
		return c, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate directories")
	}
	return "", lastErr
}

// ResolveDirs picks state and runtime directories, honouring explicit
// overrides and then XDG conventions before falling back to /tmp.
func ResolveDirs(stateOverride, runtimeOverride string) (Dirs, error) {
	var d Dirs
	var err error

	stateCandidates := []string{stateOverride, "/var/lib/" + appName}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		stateCandidates = append(stateCandidates, filepath.Join(x, appName))
	}
	if h, herr := os.UserHomeDir(); herr == nil {
		stateCandidates = append(stateCandidates, filepath.Join(h, ".local", "state", appName))
	}
	stateCandidates = append(stateCandidates, filepath.Join(os.TempDir(), appName))
	if d.State, err = firstWritableDir(stateCandidates); err != nil {
		return d, fmt.Errorf("no writable state directory: %w", err)
	}
	Debugf("paths", "state dir %s (candidates: %v)", d.State, stateCandidates)

	runtimeCandidates := []string{runtimeOverride}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		runtimeCandidates = append(runtimeCandidates, filepath.Join(x, appName))
	}
	runtimeCandidates = append(runtimeCandidates,
		"/run/"+appName,
		filepath.Join(os.TempDir(), appName+"-run"),
	)
	if d.Runtime, err = firstWritableDir(runtimeCandidates); err != nil {
		return d, fmt.Errorf("no writable runtime directory: %w", err)
	}
	Debugf("paths", "runtime dir %s (candidates: %v)", d.Runtime, runtimeCandidates)
	return d, nil
}

func (d Dirs) StateFile() string   { return filepath.Join(d.State, "last-scan.json") }
func (d Dirs) JournalFile() string { return filepath.Join(d.State, "journal.jsonl") }
func (d Dirs) LockFile() string    { return filepath.Join(d.Runtime, "lock") }
func (d Dirs) MountRoot() string   { return filepath.Join(d.Runtime, "mnt") }
