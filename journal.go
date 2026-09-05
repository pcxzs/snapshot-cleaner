package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// JournalEntry records one attempted deletion. Unlinking a file inside a
// snapshot cannot be undone, so this append-only log is the only record that
// the file ever existed there.
type JournalEntry struct {
	Time       time.Time `json:"time"`
	Provider   string    `json:"provider"`
	Pair       string    `json:"pair"`
	SnapshotID string    `json:"snapshot_id"`
	Snapshot   string    `json:"snapshot_root"`
	Path       string    `json:"path"`
	RelPath    string    `json:"rel_path"`
	Size       uint64    `json:"size"`
	Estimated  uint64    `json:"estimated_reclaim"`
	Result     string    `json:"result"`
	Error      string    `json:"error,omitempty"`
}

// Journal appends deletion records.
type Journal struct {
	path string
	f    *os.File
}

func OpenJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Journal{path: path, f: f}, nil
}

// Append writes one entry and flushes it to disk immediately, so a crash
// mid-purge still leaves a record of what was already removed.
func (j *Journal) Append(e JournalEntry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(data, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *Journal) Close() error { return j.f.Close() }

// ReadJournal returns the last n entries, or all of them when n <= 0.
func ReadJournal(path string, n int) ([]JournalEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// RenderJournal prints journal entries in reverse-chronological order.
func RenderJournal(w io.Writer, entries []JournalEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No deletions recorded.")
		return
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		status := e.Result
		if e.Error != "" {
			status += ": " + e.Error
		}
		fmt.Fprintf(w, "%s  %-8s  %s  [%s/%s]  %s\n",
			e.Time.Format("2006-01-02 15:04:05"), status,
			FormatBytes(e.Size), e.Pair, e.SnapshotID, e.RelPath)
	}
}
