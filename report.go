package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"
)

// stateVersion guards against reading a state file written by a version whose
// fingerprints or fields mean something different.
const stateVersion = 1

// ScanState is what a scan writes and what purge reads back.
type ScanState struct {
	Version      int         `json:"version"`
	Tool         string      `json:"tool"`
	ScannedAt    time.Time   `json:"scanned_at"`
	FilesystemID string      `json:"filesystem_id"`
	MinSize      uint64      `json:"min_size"`
	Costed       int         `json:"costed"`
	FreeBytes    uint64      `json:"free_bytes"`
	Pairs        []Pair      `json:"pairs"`
	Candidates   []Candidate `json:"candidates"`

	// Folders is set by a folder-view scan and empty otherwise. The two views
	// are alternatives: a folder scan collects files far below any threshold
	// worth listing individually, so it records the rollups and lets purge
	// rebuild a folder's members from the snapshots when it needs them.
	Folders []Folder `json:"folders,omitempty"`
	// FolderMinSize is the rollup threshold a folder scan used, so the report
	// can say what it filtered on.
	FolderMinSize uint64 `json:"folder_min_size,omitempty"`
}

// FolderView reports whether this scan ranked folders rather than files.
func (st *ScanState) FolderView() bool { return len(st.Folders) > 0 || st.FolderMinSize > 0 }

// SaveState writes the state file atomically, so an interrupted write cannot
// leave purge reading a truncated candidate list.
func SaveState(path string, st *ScanState) error {
	st.Version = stateVersion
	st.Tool = appName
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadState reads a previous scan.
func LoadState(path string) (*ScanState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no scan found - run `%s scan` first", appName)
		}
		return nil, err
	}
	var st ScanState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", path, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("%s was written by a different version - run `%s scan` again", path, appName)
	}
	return &st, nil
}

// Find returns the candidate with the given display id.
func (st *ScanState) Find(id int) (*Candidate, bool) {
	for i := range st.Candidates {
		if st.Candidates[i].ID == id {
			return &st.Candidates[i], true
		}
	}
	return nil, false
}

// RenderTable prints the ranked candidate table.
func RenderTable(w io.Writer, st *ScanState, top int, showAll bool) {
	cands := st.Candidates
	if top > 0 && top < len(cands) {
		cands = cands[:top]
	}
	if len(cands) == 0 {
		fmt.Fprintf(w, "No pinned files found at or above %s.\n", FormatBytes(st.MinSize))
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tRECLAIM\tAPPARENT\tSNAPS\tKIND\tSUBVOL\tPATH")

	var total uint64
	for _, c := range cands {
		total += c.Usage.Bytes
		reclaim := FormatBytes(c.Usage.Bytes)
		if c.Usage.Approx() {
			reclaim = "~" + reclaim
		}
		if c.Usage.Method == MethodNone {
			reclaim = "?"
		}
		path := c.RelPath
		if !showAll {
			path = Truncate(path, 60)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d/%d\t%s\t%s\t%s\n",
			c.ID, reclaim, FormatBytes(c.Apparent), len(c.Copies), c.TotalIn, c.Kind, c.Pair, path)
	}
	tw.Flush()

	fmt.Fprintf(w, "\nTotal shown: %s reclaimable across %d file(s)", FormatBytes(total), len(cands))
	if len(st.Candidates) > len(cands) {
		fmt.Fprintf(w, " (%d more not shown; use --top 0)", len(st.Candidates)-len(cands))
	}
	fmt.Fprintf(w, "\nFree now: %s\n", FormatBytes(st.FreeBytes))

	renderFooterNotes(w, st, cands)
}

// renderFooterNotes states the assumptions behind the numbers. These belong in
// the output rather than only the documentation: the figures look authoritative
// and the reader deserves to know what they rest on.
func renderFooterNotes(w io.Writer, st *ScanState, shown []Candidate) {
	approx, compressed, unmeasured, overApparent := 0, 0, 0, 0
	fiemapUsed := false
	for _, c := range shown {
		// Only count rows where the reader can actually see the difference.
		// Most files exceed their apparent size by a few bytes of extent
		// rounding, which the formatted column hides; claiming "6 rows show
		// RECLAIM above APPARENT" when five of them read as equal makes the
		// note contradict the table it is explaining.
		if c.Usage.Method != MethodNone && c.Usage.Bytes > c.Apparent &&
			FormatBytes(c.Usage.Bytes) != FormatBytes(c.Apparent) {
			overApparent++
		}
		if c.Usage.Method == MethodNone {
			// Rendered as "?", not as a "~" estimate; counting it as one would
			// misdescribe what the reader is looking at.
			unmeasured++
			continue
		}
		if c.Usage.Approx() {
			approx++
		}
		if c.Usage.Compressed {
			compressed++
		}
		if c.Usage.Method == MethodFiemap {
			fiemapUsed = true
		}
	}

	fmt.Fprintln(w, "\nNotes:")
	fmt.Fprintln(w, "  RECLAIM is the disk space freed by removing the file from ALL the snapshots")
	fmt.Fprintln(w, "  holding it. Removing it from only some of them frees close to nothing.")
	if approx > 0 {
		fmt.Fprintf(w, "  %d row(s) marked ~ are estimates.", approx)
		if fiemapUsed && compressed > 0 {
			fmt.Fprint(w, " Measured via FIEMAP over compressed extents,\n  which reports logical length, so those figures are upper bounds.")
		}
		fmt.Fprintln(w)
	}
	if overApparent > 0 {
		// A row where RECLAIM exceeds APPARENT looks wrong at a glance, so
		// explain it rather than leaving the reader to distrust the whole
		// table. Repeatedly rewritten files (VM images, databases) reference
		// only parts of larger extents; the dead remainder is still allocated
		// and is released along with the last reference to the extent.
		fmt.Fprintf(w, "  %d row(s) show RECLAIM above APPARENT. That is expected for files rewritten\n", overApparent)
		fmt.Fprintln(w, "  in place, such as VM images: they reference parts of larger extents whose")
		fmt.Fprintln(w, "  unused remainder is still allocated, and is freed with the last reference.")
	}
	if unmeasured > 0 {
		fmt.Fprintf(w, "  %d row(s) shown as ? were not measured; raise --cost-limit to include them.\n", unmeasured)
	}
	if st.Costed < len(st.Candidates) {
		fmt.Fprintf(w, "  Only the %d largest of %d candidates were measured (--cost-limit).\n",
			st.Costed, len(st.Candidates))
	}
	fmt.Fprintln(w, "  Figures assume nothing outside these snapshots references the same extents.")
}

// RenderFolderTable prints the ranked folder rollups.
func RenderFolderTable(w io.Writer, st *ScanState, top int, showAll bool) {
	folders := st.Folders
	if top > 0 && top < len(folders) {
		folders = folders[:top]
	}
	if len(folders) == 0 {
		fmt.Fprintf(w, "No pinned folders found at or above %s.\n", FormatBytes(st.FolderMinSize))
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tRECLAIM\tAPPARENT\tFILES\tSNAPS\tKIND\tSUBVOL\tPATH")

	var total uint64
	for _, f := range folders {
		total += f.Usage.Bytes
		reclaim := FormatBytes(f.Usage.Bytes)
		if f.Usage.Approx() {
			reclaim = "~" + reclaim
		}
		if f.Usage.Method == MethodNone {
			reclaim = "?"
		}
		path := f.RelPath
		if !showAll {
			path = Truncate(path, 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d/%d\t%s\t%s\t%s\n",
			f.Label(), reclaim, FormatBytes(f.Apparent), f.Files, f.Snaps(), f.TotalIn,
			f.Kind, f.Pair, path)
	}
	tw.Flush()

	var files int
	for _, f := range folders {
		files += f.Files
	}
	fmt.Fprintf(w, "\nTotal shown: %s reclaimable across %d folder(s) holding %d file(s)",
		FormatBytes(total), len(folders), files)
	if len(st.Folders) > len(folders) {
		fmt.Fprintf(w, " (%d more not shown; use --top 0)", len(st.Folders)-len(folders))
	}
	fmt.Fprintf(w, "\nFree now: %s\n", FormatBytes(st.FreeBytes))

	renderFolderNotes(w, st, folders)
}

// renderFolderNotes states what the folder figures rest on. The rollup makes a
// couple of new promises the file view does not, and they belong next to the
// numbers rather than only in the documentation.
func renderFolderNotes(w io.Writer, st *ScanState, shown []Folder) {
	sampled, unmeasured, thinned, overApparent := 0, 0, 0, 0
	for _, f := range shown {
		switch {
		case f.Usage.Method == MethodNone:
			unmeasured++
		case f.Usage.Method == MethodSampled:
			sampled++
		}
		if f.Kind == FolderThinned {
			thinned++
		}
		// Only count rows where the reader can see the difference, as the file
		// view does: a note claiming rows exceed their apparent size reads as
		// wrong when the two columns are formatted identically.
		if f.Usage.Method != MethodNone && f.Usage.Bytes > f.Apparent &&
			FormatBytes(f.Usage.Bytes) != FormatBytes(f.Apparent) {
			overApparent++
		}
	}

	fmt.Fprintln(w, "\nNotes:")
	fmt.Fprintln(w, "  RECLAIM is the disk space freed by removing the whole folder from ALL the")
	fmt.Fprintln(w, "  snapshots holding it, counting each shared extent once. FILES is how many")
	fmt.Fprintln(w, "  pinned files it contains; none of them need be large on its own.")
	fmt.Fprintln(w, "  A 'deleted' folder is gone from the live filesystem entirely, and the row")
	fmt.Fprintln(w, "  is the topmost directory that is missing, not each subdirectory under it.")
	if thinned > 0 {
		fmt.Fprintf(w, "  %d row(s) marked 'thinned' still exist live; only files deleted out of them\n", thinned)
		fmt.Fprintln(w, "  are counted, and purging one leaves the live directory untouched.")
	}
	if overApparent > 0 {
		// Far more common here than in the file view: every one of a folder's
		// thousands of files rounds up to a whole block, and the rounding adds
		// up to something visible where on a single file it never is.
		fmt.Fprintf(w, "  %d row(s) show RECLAIM above APPARENT. Each file occupies whole blocks, so\n", overApparent)
		fmt.Fprintln(w, "  a folder of many small files allocates more than their sizes add up to, and")
		fmt.Fprintln(w, "  all of it is freed. Files rewritten in place hold extra extents the same way.")
	}
	if sampled > 0 {
		fmt.Fprintf(w, "  %d row(s) were too large to open file by file, so they were measured from\n", sampled)
		fmt.Fprintln(w, "  an evenly spaced sample and scaled by apparent size. Those are estimates;")
		fmt.Fprintln(w, "  raise --folder-sample to narrow them.")
	}
	if unmeasured > 0 {
		fmt.Fprintf(w, "  %d row(s) shown as ? were not measured; raise --cost-limit to include them.\n", unmeasured)
	}
	if st.Costed < len(st.Folders) {
		fmt.Fprintf(w, "  Only the %d largest of %d folders were measured (--cost-limit).\n",
			st.Costed, len(st.Folders))
	}
	fmt.Fprintln(w, "  Figures assume nothing outside these snapshots references the same extents.")
}

// RenderJSON writes the machine-readable form.
func RenderJSON(w io.Writer, st *ScanState) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

// RenderSnapshots prints the detected pairs and their snapshots.
func RenderSnapshots(w io.Writer, pairs []Pair) {
	if len(pairs) == 0 {
		fmt.Fprintln(w, "No snapshots detected.")
		return
	}
	for _, p := range pairs {
		fmt.Fprintf(w, "%s  (provider: %s, live: %s, %d snapshot(s))\n", p.Name, p.Provider, p.Live, len(p.Snapshots))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ID\tCREATED\tTAG\tMODE\tROOT")
		for _, s := range p.Snapshots {
			created := "unknown"
			if !s.Created.IsZero() {
				created = s.Created.Format("2006-01-02 15:04:05")
			}
			mode := "rw"
			if s.ReadOnly {
				mode = "ro"
			}
			if s.ReceivedUUID != "" {
				mode += ",received"
			}
			tag := s.Tag
			if tag == "" {
				tag = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", s.ID, created, tag, mode, s.Root)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
}

// Relocate rewrites the snapshot paths recorded by a previous scan to match
// where those snapshots are reachable now.
//
// This is required, not cosmetic: snapshots are usually reached through a
// temporary mount of the btrfs top level, and that mount point is different in
// every process. Paths saved by `scan` are therefore stale by the time `purge`
// runs. Snapshots are re-identified by (pair name, snapshot id), both of which
// are stable, and any snapshot that has since rotated away is dropped.
func (st *ScanState) Relocate(current []Pair) (dropped int) {
	roots := map[string]map[string]Snapshot{}
	live := map[string]string{}
	for _, p := range current {
		if roots[p.Name] == nil {
			roots[p.Name] = map[string]Snapshot{}
		}
		for _, s := range p.Snapshots {
			roots[p.Name][s.ID] = s
		}
		live[p.Name] = p.Live
	}

	for i := range st.Candidates {
		c := &st.Candidates[i]
		if l, ok := live[c.Pair]; ok {
			c.Live = l
			c.LivePath = filepath.Join(l, c.RelPath)
		}
		kept := c.Copies[:0]
		for _, cp := range c.Copies {
			snap, ok := roots[c.Pair][cp.SnapshotID]
			if !ok {
				dropped++
				continue
			}
			cp.Snapshot = snap.Root
			cp.Path = filepath.Join(snap.Root, c.RelPath)
			kept = append(kept, cp)
		}
		c.Copies = kept
	}

	// Folder rollups need the same treatment: their holder roots were recorded
	// under the mount this process does not have.
	for i := range st.Folders {
		f := &st.Folders[i]
		if l, ok := live[f.Pair]; ok {
			f.Live = l
			f.LivePath = filepath.Join(l, f.RelPath)
		}
		kept := f.Holders[:0]
		for _, h := range f.Holders {
			snap, ok := roots[f.Pair][h.SnapshotID]
			if !ok {
				dropped++
				continue
			}
			h.Root = snap.Root
			kept = append(kept, h)
		}
		f.Holders = kept
	}
	st.Pairs = current
	return dropped
}
