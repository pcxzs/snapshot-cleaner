package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// PurgeTarget is one file in one snapshot, queued for removal.
type PurgeTarget struct {
	Candidate *Candidate
	Copy      Copy
	Snapshot  Snapshot
}

// Skip records a target or candidate that will not be touched, and why. Skips
// are always reported: silently dropping a target the user selected would be
// worse than doing nothing.
type Skip struct {
	What   string
	Reason string
}

// PurgePlan is the validated set of removals.
type PurgePlan struct {
	Targets   []PurgeTarget
	Skips     []Skip
	Estimated uint64
	Partial   bool
}

// ByCandidate groups the plan for display and for the "all holders or none"
// rule that makes a purge actually free space.
func (p *PurgePlan) ByCandidate() map[int][]PurgeTarget {
	out := map[int][]PurgeTarget{}
	for _, t := range p.Targets {
		out[t.Candidate.ID] = append(out[t.Candidate.ID], t)
	}
	return out
}

// BuildPlan re-validates the selected candidates against the filesystem as it
// is now. A scan may be hours old and snapshot managers rotate on timers, so
// nothing recorded at scan time is trusted without being checked again.
func BuildPlan(st *ScanState, ids []int, partial bool) (*PurgePlan, error) {
	snapByRoot := map[string]Snapshot{}
	for _, p := range st.Pairs {
		for _, s := range p.Snapshots {
			snapByRoot[s.Root] = s
		}
	}

	Infof("plan", "building plan for ids=%v partial=%v from a scan of %s",
		ids, partial, st.ScannedAt.Format(time.RFC3339))
	plan := &PurgePlan{Partial: partial}
	for _, id := range ids {
		cand, ok := st.Find(id)
		if !ok {
			Warnf("plan", "id %d: not present in the last scan", id)
			plan.Skips = append(plan.Skips, Skip{fmt.Sprintf("id %d", id), "no such candidate in the last scan"})
			continue
		}
		Infof("plan", "id %d: %s (%s) kind=%s copies=%d/%d reclaim=%s",
			id, cand.RelPath, cand.Pair, cand.Kind, len(cand.Copies), cand.TotalIn,
			FormatBytes(cand.Usage.Bytes))

		// A file that has come back on the live tree is a different decision
		// than the one the user reviewed.
		if cand.Kind == KindDeleted {
			if _, err := os.Lstat(cand.LivePath); err == nil {
				Warnf("plan", "id %d: %s exists on the live filesystem again; skipping", id, cand.LivePath)
				plan.Skips = append(plan.Skips, Skip{cand.RelPath, "file exists on the live filesystem again"})
				continue
			}
			Debugf("plan", "id %d: confirmed absent from live at %s", id, cand.LivePath)
		}

		var targets []PurgeTarget
		bad := false
		for _, c := range cand.Copies {
			snap, ok := snapByRoot[c.Snapshot]
			if !ok {
				Warnf("plan", "id %d: snapshot root %s not in state; skipping copy", id, c.Snapshot)
				plan.Skips = append(plan.Skips, Skip{c.Path, "snapshot is no longer in the scan state"})
				bad = true
				continue
			}
			if snap.ReceivedUUID != "" {
				Warnf("plan", "id %d: snapshot %s has received_uuid=%s; refusing", id, snap.ID, snap.ReceivedUUID)
				plan.Skips = append(plan.Skips, Skip{c.Path,
					"snapshot was received via btrfs send; clearing read-only would break its send/receive chain"})
				bad = true
				continue
			}

			var stt unix.Stat_t
			if err := unix.Lstat(c.Path, &stt); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// The snapshot rotated away since the scan. The remaining
					// copies are still the complete current holder set, because
					// a snapshot taken after the file was deleted cannot hold it.
					Infof("plan", "id %d: copy in %s already gone (rotated out)", id, snap.ID)
					plan.Skips = append(plan.Skips, Skip{c.Path, "already gone (snapshot rotated out)"})
					continue
				}
				Errorf("plan", "id %d: cannot stat %s: %v", id, c.Path, err)
				plan.Skips = append(plan.Skips, Skip{c.Path, "cannot stat: " + err.Error()})
				bad = true
				continue
			}
			if stt.Mode&unix.S_IFMT != unix.S_IFREG {
				plan.Skips = append(plan.Skips, Skip{c.Path, "no longer a regular file"})
				bad = true
				continue
			}
			if stt.Ino != c.Ino || uint64(stt.Size) != c.Size || stt.Mtim.Nano() != c.MtimeNs {
				Warnf("plan", "id %d: %s changed since the scan (ino %d->%d size %d->%d mtime %d->%d)",
					id, c.Path, c.Ino, stt.Ino, c.Size, stt.Size, c.MtimeNs, stt.Mtim.Nano())
				plan.Skips = append(plan.Skips, Skip{c.Path, "changed since the scan (inode, size or mtime differ)"})
				bad = true
				continue
			}
			Debugf("plan", "id %d: validated %s (ino=%d size=%d)", id, c.Path, stt.Ino, stt.Size)
			targets = append(targets, PurgeTarget{Candidate: cand, Copy: c, Snapshot: snap})
		}

		if len(targets) == 0 {
			continue
		}
		// Removing a file from only some of the snapshots holding it frees
		// essentially nothing, so a candidate with an unusable copy is skipped
		// entirely unless the user explicitly asked for partial work.
		if bad && !partial {
			Warnf("plan", "id %d: dropping entirely, %d copy/copies unvalidated and --partial not set",
				id, len(cand.Copies)-len(targets))
			plan.Skips = append(plan.Skips, Skip{cand.RelPath,
				"skipped entirely: some copies could not be validated (use --partial to override)"})
			continue
		}
		plan.Targets = append(plan.Targets, targets...)
		plan.Estimated += cand.Usage.Bytes
	}

	Infof("plan", "plan complete: %d target(s), %d skip(s), estimated reclaim %s",
		len(plan.Targets), len(plan.Skips), FormatBytes(plan.Estimated))
	sort.Slice(plan.Targets, func(i, j int) bool {
		if plan.Targets[i].Copy.Snapshot != plan.Targets[j].Copy.Snapshot {
			return plan.Targets[i].Copy.Snapshot < plan.Targets[j].Copy.Snapshot
		}
		return plan.Targets[i].Copy.Path < plan.Targets[j].Copy.Path
	})
	return plan, nil
}

// RenderPlan prints exactly what will be removed.
func RenderPlan(w io.Writer, plan *PurgePlan, apply bool) {
	if len(plan.Skips) > 0 {
		fmt.Fprintln(w, "Skipped:")
		for _, s := range plan.Skips {
			fmt.Fprintf(w, "  %s\n    %s\n", s.What, s.Reason)
		}
		fmt.Fprintln(w)
	}
	if len(plan.Targets) == 0 {
		fmt.Fprintln(w, "Nothing to do.")
		return
	}

	byCand := plan.ByCandidate()
	ids := make([]int, 0, len(byCand))
	for id := range byCand {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	verb := "Would remove"
	if apply {
		verb = "Will remove"
	}
	fmt.Fprintf(w, "%s %d file copy/copies from %d snapshot(s):\n\n", verb, len(plan.Targets), countSnapshots(plan))
	for _, id := range ids {
		ts := byCand[id]
		c := ts[0].Candidate
		fmt.Fprintf(w, "  [%d] %s\n", id, c.RelPath)
		fmt.Fprintf(w, "       subvol %s, %s apparent, reclaim %s\n",
			c.Pair, FormatBytes(c.Apparent), FormatBytes(c.Usage.Bytes))
		for _, t := range ts {
			fmt.Fprintf(w, "       - %s\n", t.Copy.Path)
		}
		if len(ts) < len(c.Copies) {
			fmt.Fprintf(w, "       ! only %d of %d holders - expect little or no space to be freed\n",
				len(ts), len(c.Copies))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Estimated reclaim: %s\n", FormatBytes(plan.Estimated))
}

func countSnapshots(plan *PurgePlan) int {
	seen := map[string]bool{}
	for _, t := range plan.Targets {
		seen[t.Copy.Snapshot] = true
	}
	return len(seen)
}

// Purger executes a plan.
type Purger struct {
	Journal *Journal
	Out     io.Writer
	Mounter *Mounter
	// MountPoint is the mount this tool created that must be writable during
	// the purge; empty when the snapshots are reached through a system mount.
	MountPoint string
}

// Execute performs the removals. Every snapshot it makes writable is restored
// to read-only, from a deferred call here and from the signal handler, and the
// flags are verified afterwards.
func (p *Purger) Execute(plan *PurgePlan) (reclaimed int, err error) {
	if len(plan.Targets) == 0 {
		return 0, nil
	}

	Infof("purge", "executing: %d target(s) across %d snapshot(s), mountpoint=%q",
		len(plan.Targets), countSnapshots(plan), p.MountPoint)

	if p.MountPoint != "" {
		if err := p.Mounter.Remount(p.MountPoint, false); err != nil {
			Errorf("purge", "could not make %s writable: %v", p.MountPoint, err)
			return 0, fmt.Errorf("making snapshots writable: %w", err)
		}
		defer func() {
			// Best effort only. btrfs refuses to remount one mount read-only
			// while other mounts of the same filesystem are read-write, which
			// is always true here because / and /home are mounted rw. The
			// mount is private to this process and is unmounted moments later
			// by cleanup, so failing to restore the flag changes nothing -
			// reporting it as an error would make a successful purge look
			// like it failed.
			if rerr := p.Mounter.Remount(p.MountPoint, true); rerr != nil {
				Debugf("purge", "could not restore read-only on our own mount %s: %v "+
					"(harmless; it is unmounted next)", p.MountPoint, rerr)
			}
		}()
	}

	bySnapshot := map[string][]PurgeTarget{}
	var order []string
	for _, t := range plan.Targets {
		if _, seen := bySnapshot[t.Copy.Snapshot]; !seen {
			order = append(order, t.Copy.Snapshot)
		}
		bySnapshot[t.Copy.Snapshot] = append(bySnapshot[t.Copy.Snapshot], t)
	}

	var failures []string
	for _, root := range order {
		n, err := p.purgeOneSnapshot(root, bySnapshot[root])
		reclaimed += n
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", root, err))
		}
	}

	if verr := p.verifyFlags(order); verr != nil {
		Errorf("purge", "flag verification failed: %v", verr)
		failures = append(failures, verr.Error())
	} else {
		Infof("purge", "verified: all %d touched snapshot(s) are back to their original flags", len(order))
	}
	if len(failures) > 0 {
		Errorf("purge", "finished with %d failure(s): %s", len(failures), strings.Join(failures, "; "))
		return reclaimed, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	Infof("purge", "removed %d file copy/copies successfully", reclaimed)
	return reclaimed, nil
}

// purgeOneSnapshot unlinks every target inside a single snapshot, holding the
// snapshot writable for the shortest possible window.
func (p *Purger) purgeOneSnapshot(root string, targets []PurgeTarget) (n int, err error) {
	wasReadOnly, err := IsReadOnlySubvol(root)
	if err != nil {
		Errorf("purge", "cannot read flags of %s: %v", root, err)
		return 0, fmt.Errorf("reading subvolume flags: %w", err)
	}
	Infof("purge", "snapshot %s: %d target(s), currently %s", root, len(targets),
		map[bool]string{true: "read-only", false: "writable"}[wasReadOnly])

	if wasReadOnly {
		if err := SetSubvolReadOnly(root, false); err != nil {
			Errorf("purge", "cannot clear read-only on %s: %v", root, err)
			return 0, fmt.Errorf("clearing read-only: %w", err)
		}
		Debugf("purge", "cleared read-only on %s", root)
		registerReseal(root)
		defer func() {
			// Restoring the flag matters more than any error above it: a
			// snapshot left writable is the real hazard in this tool.
			if rerr := SetSubvolReadOnly(root, true); rerr != nil {
				Errorf("purge", "RESTORE READ-ONLY FAILED for %s: %v", root, rerr)
				err = errors.Join(err, fmt.Errorf("RESTORE READ-ONLY FAILED for %s: %w", root, rerr))
				return
			}
			Debugf("purge", "restored read-only on %s", root)
			unregisterReseal(root)
		}()
	}

	for _, t := range targets {
		entry := JournalEntry{
			Provider:   t.Candidate.Provider,
			Pair:       t.Candidate.Pair,
			SnapshotID: t.Snapshot.ID,
			Snapshot:   root,
			Path:       t.Copy.Path,
			RelPath:    t.Candidate.RelPath,
			Size:       t.Copy.Size,
			Estimated:  t.Candidate.Usage.Bytes,
		}
		if uerr := safeUnlink(root, t.Candidate.RelPath, t.Copy); uerr != nil {
			Errorf("purge", "unlink FAILED %s: %v", t.Copy.Path, uerr)
			Count("purge.failed", 1)
			entry.Result = "failed"
			entry.Error = uerr.Error()
			p.record(entry)
			fmt.Fprintf(p.Out, "  failed: %s: %v\n", t.Copy.Path, uerr)
			continue
		}
		Infof("purge", "unlinked %s (%s, ino=%d)", t.Copy.Path, FormatBytes(t.Copy.Size), t.Copy.Ino)
		Count("purge.removed", 1)
		entry.Result = "removed"
		p.record(entry)
		n++
		fmt.Fprintf(p.Out, "  removed %s\n", t.Copy.Path)
	}
	return n, nil
}

func (p *Purger) record(e JournalEntry) {
	if p.Journal == nil {
		return
	}
	if err := p.Journal.Append(e); err != nil {
		fmt.Fprintf(p.Out, "  warning: could not write journal entry: %v\n", err)
	}
}

// verifyFlags re-reads every touched snapshot and reports any left writable.
func (p *Purger) verifyFlags(roots []string) error {
	var bad []string
	for _, root := range roots {
		ro, err := IsReadOnlySubvol(root)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s (cannot read flags: %v)", root, err))
			continue
		}
		if !ro && wasSealed(root) {
			bad = append(bad, root)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("SNAPSHOTS LEFT WRITABLE, fix with `btrfs property set -ts <path> ro true`: %s",
			strings.Join(bad, ", "))
	}
	return nil
}

// safeUnlink removes rel beneath root without ever following a symlink out of
// the snapshot. Each path component is opened with O_NOFOLLOW from the previous
// directory's descriptor, so a symlink planted inside a snapshot cannot
// redirect the unlink onto the live filesystem.
func safeUnlink(root, rel string, expect Copy) error {
	rel = filepath.Clean(rel)
	if rel == "." || rel == "/" || rel == "" || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("refusing to remove %q", rel)
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("refusing absolute path %q", rel)
	}

	rootFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open snapshot root: %w", err)
	}
	defer unix.Close(rootFd)

	parts := strings.Split(rel, string(os.PathSeparator))
	dirFd := rootFd
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("refusing path component %q", part)
		}
		next, err := unix.Openat(dirFd, part,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if dirFd != rootFd {
			unix.Close(dirFd)
		}
		if err != nil {
			return fmt.Errorf("descend into %q: %w", part, err)
		}
		dirFd = next
	}
	if dirFd != rootFd {
		defer unix.Close(dirFd)
	}

	base := parts[len(parts)-1]
	if base == "" || base == "." || base == ".." {
		return fmt.Errorf("refusing basename %q", base)
	}

	// Confirm at the final descriptor that this is still the exact file the
	// plan validated, closing the window between validation and removal.
	var stt unix.Stat_t
	if err := unix.Fstatat(dirFd, base, &stt, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat target: %w", err)
	}
	if stt.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("target is not a regular file")
	}
	if stt.Ino == btrfsFirstFreeObjectID {
		return fmt.Errorf("target is a subvolume root")
	}
	if stt.Ino != expect.Ino || uint64(stt.Size) != expect.Size || stt.Mtim.Nano() != expect.MtimeNs {
		return fmt.Errorf("target changed between validation and removal")
	}

	if err := unix.Unlinkat(dirFd, base, 0); err != nil {
		return fmt.Errorf("unlink: %w", err)
	}
	return nil
}

// SettleFreeBytes waits for btrfs to finish releasing the extents just
// unlinked, then reports the free space.
//
// btrfs frees space asynchronously through delayed refs and its cleaner
// thread, so reading statfs immediately after the unlink undercounts badly: a
// real 13.4 GiB reclaim read as 7.11 GiB at the instant of removal and reached
// the full amount moments later. Reporting the instantaneous figure makes a
// correct prediction look wrong, so wait for the number to stop moving.
func SettleFreeBytes(path string, timeout time.Duration) (uint64, bool) {
	unix.Sync()

	deadline := time.Now().Add(timeout)
	last, err := FreeBytes(path)
	if err != nil {
		return 0, false
	}
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		cur, err := FreeBytes(path)
		if err != nil {
			return last, false
		}
		if cur == last {
			// Two readings in a row agreeing means the cleaner has caught up.
			if stable++; stable >= 2 {
				return cur, true
			}
		} else {
			stable = 0
		}
		last = cur
	}
	return last, false
}
