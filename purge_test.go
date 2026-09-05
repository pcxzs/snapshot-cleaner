package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fingerprint builds the Copy record safeUnlink validates against.
func fingerprint(t *testing.T, path string) Copy {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return Copy{Path: path, Ino: st.Ino, Size: uint64(st.Size), MtimeNs: st.Mtim.Nano()}
}

func TestSafeUnlinkRemovesTheRightFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "a", "b", "big.iso")
	if err := os.WriteFile(target, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := safeUnlink(root, "a/b/big.iso", fingerprint(t, target)); err != nil {
		t.Fatalf("safeUnlink: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("file was not removed")
	}
}

// A symlinked path component must never be followed: a symlink planted inside
// a snapshot could otherwise redirect the unlink onto the live filesystem.
func TestSafeUnlinkRefusesToFollowSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "snapshot")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(victim, []byte("do not delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	// snapshot/escape -> ../outside
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	err := safeUnlink(root, "escape/important.txt", fingerprint(t, victim))
	if err == nil {
		t.Fatal("safeUnlink followed a symlinked directory out of the snapshot")
	}
	if _, statErr := os.Lstat(victim); statErr != nil {
		t.Fatalf("the file outside the snapshot was removed: %v", statErr)
	}
}

func TestSafeUnlinkRefusesSymlinkTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "snapshot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "important.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.iso")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	if err := safeUnlink(root, "link.iso", fingerprint(t, victim)); err == nil {
		t.Fatal("safeUnlink accepted a symlink as its target")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatal("the symlink target was removed")
	}
}

func TestSafeUnlinkRefusesTraversalAndAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"..", "../etc/passwd", "/etc/passwd", ".", "", "a/../../x"} {
		if err := safeUnlink(root, rel, Copy{}); err == nil {
			t.Errorf("safeUnlink accepted %q", rel)
		}
	}
}

// The fingerprint check is what stops a stale scan from removing a file that
// has changed since it was reviewed.
func TestSafeUnlinkRefusesChangedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "file.bin")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp := fingerprint(t, target)
	fp.Size = 999999 // pretend the scan saw a different file

	if err := safeUnlink(root, "file.bin", fp); err == nil {
		t.Fatal("safeUnlink removed a file whose fingerprint did not match")
	}
	if !strings.Contains(mustErr(t, safeUnlink(root, "file.bin", fp)), "changed") {
		t.Error("error should explain that the target changed")
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatal("the file was removed despite the mismatch")
	}
}

func mustErr(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}

// BuildPlan must reject anything it cannot re-verify, rather than trusting the
// scan state.
func TestBuildPlanRevalidates(t *testing.T) {
	root := t.TempDir()
	snapRoot := filepath.Join(root, "snap1")
	if err := os.MkdirAll(snapRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(snapRoot, "big.iso")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp := fingerprint(t, file)

	newState := func(mutate func(*ScanState)) *ScanState {
		st := &ScanState{
			Pairs: []Pair{{
				Name: "home", Live: filepath.Join(root, "live"),
				Snapshots: []Snapshot{{ID: "snap1", Root: snapRoot, ReadOnly: true}},
			}},
			Candidates: []Candidate{{
				ID: 1, Pair: "home", RelPath: "big.iso",
				LivePath: filepath.Join(root, "live", "big.iso"),
				Kind:     KindDeleted, Apparent: 4, TotalIn: 1,
				Copies: []Copy{{
					SnapshotID: "snap1", Snapshot: snapRoot, Path: file,
					Ino: fp.Ino, Size: fp.Size, MtimeNs: fp.MtimeNs,
				}},
			}},
		}
		if mutate != nil {
			mutate(st)
		}
		return st
	}

	t.Run("accepts a valid target", func(t *testing.T) {
		plan, err := BuildPlan(newState(nil), []int{1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Targets) != 1 {
			t.Fatalf("want 1 target, got %d (skips: %+v)", len(plan.Targets), plan.Skips)
		}
	})

	t.Run("skips when the fingerprint no longer matches", func(t *testing.T) {
		st := newState(func(s *ScanState) { s.Candidates[0].Copies[0].Size = 12345 })
		plan, err := BuildPlan(st, []int{1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Targets) != 0 {
			t.Fatal("planned a removal for a file that changed since the scan")
		}
		if len(plan.Skips) == 0 {
			t.Fatal("skips must be reported, not silent")
		}
	})

	t.Run("skips when the live file has come back", func(t *testing.T) {
		st := newState(nil)
		if err := os.MkdirAll(filepath.Join(root, "live"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(st.Candidates[0].LivePath, []byte("restored"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(st.Candidates[0].LivePath)

		plan, err := BuildPlan(st, []int{1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Targets) != 0 {
			t.Fatal("planned a removal although the file exists on the live filesystem again")
		}
	})

	t.Run("refuses a received snapshot", func(t *testing.T) {
		st := newState(func(s *ScanState) { s.Pairs[0].Snapshots[0].ReceivedUUID = "abcd" })
		plan, err := BuildPlan(st, []int{1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Targets) != 0 {
			t.Fatal("planned a removal inside a received snapshot")
		}
	})

	t.Run("reports an unknown id", func(t *testing.T) {
		plan, err := BuildPlan(newState(nil), []int{99}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Targets) != 0 || len(plan.Skips) != 1 {
			t.Fatalf("targets=%d skips=%d", len(plan.Targets), len(plan.Skips))
		}
	})
}

func TestSettleFreeBytesReturnsAReading(t *testing.T) {
	// On an idle filesystem the reading stabilises immediately; the point of
	// the test is that it terminates and reports something sane rather than
	// spinning for the whole timeout.
	start := time.Now()
	got, settled := SettleFreeBytes(t.TempDir(), 5*time.Second)
	if got == 0 {
		t.Error("expected a non-zero free-space reading")
	}
	if !settled {
		t.Error("an idle filesystem should settle well inside the timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, should have returned as soon as the reading stabilised", elapsed)
	}
}
