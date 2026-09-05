package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
)

// The tool takes two kinds of temporary liberty with the system: it mounts the
// btrfs top level, and it clears the read-only flag on a snapshot for the
// duration of an unlink. Both must be undone even if the process is
// interrupted, so both are tracked globally and unwound from a signal handler
// as well as from the normal deferred paths.

var (
	resealMu  sync.Mutex
	resealSet = map[string]bool{}

	cleanupMu    sync.Mutex
	cleanupFuncs []func()
	cleanupOnce  sync.Once
)

// registerReseal notes that a snapshot has been made writable by us.
func registerReseal(root string) {
	resealMu.Lock()
	defer resealMu.Unlock()
	resealSet[root] = true
}

// unregisterReseal notes that a snapshot has been successfully re-sealed.
func unregisterReseal(root string) {
	resealMu.Lock()
	defer resealMu.Unlock()
	delete(resealSet, root)
}

// wasSealed reports whether we cleared this snapshot's read-only flag and have
// not yet restored it.
func wasSealed(root string) bool {
	resealMu.Lock()
	defer resealMu.Unlock()
	return resealSet[root]
}

// ResealAll restores the read-only flag on every snapshot still marked as
// ours. Safe to call repeatedly.
func ResealAll() []error {
	resealMu.Lock()
	roots := make([]string, 0, len(resealSet))
	for r := range resealSet {
		roots = append(roots, r)
	}
	resealMu.Unlock()

	if len(roots) > 0 {
		Warnf("cleanup", "re-sealing %d snapshot(s) left writable", len(roots))
	}
	var errs []error
	for _, r := range roots {
		if err := SetSubvolReadOnly(r, true); err != nil {
			Errorf("cleanup", "could not restore read-only on %s: %v", r, err)
			errs = append(errs, fmt.Errorf("%s: %w", r, err))
			continue
		}
		Infof("cleanup", "restored read-only on %s", r)
		unregisterReseal(r)
	}
	return errs
}

// RegisterCleanup adds a function to run on normal exit and on signals.
func RegisterCleanup(f func()) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanupFuncs = append(cleanupFuncs, f)
}

// RunCleanup unwinds everything registered, most recent first. It runs at most
// once, so the deferred call and the signal handler cannot collide.
func RunCleanup() {
	cleanupOnce.Do(func() {
		if errs := ResealAll(); len(errs) > 0 {
			for _, err := range errs {
				fmt.Fprintf(os.Stderr,
					"\nERROR: could not restore read-only flag: %v\n"+
						"Fix it with: btrfs property set -ts <path> ro true\n", err)
			}
		}
		cleanupMu.Lock()
		fns := cleanupFuncs
		cleanupFuncs = nil
		cleanupMu.Unlock()

		for i := len(fns) - 1; i >= 0; i-- {
			fns[i]()
		}
	})
}

// InstallSignalHandler makes Ctrl-C and SIGTERM unwind cleanly rather than
// leaving a snapshot writable or a mount behind.
func InstallSignalHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, unix.SIGTERM, unix.SIGHUP)
	go func() {
		sig := <-ch
		Warnf("signal", "received %s, unwinding", sig)
		fmt.Fprintf(os.Stderr, "\nInterrupted (%s), cleaning up...\n", sig)
		RunCleanup()
		os.Exit(130)
	}()
}
