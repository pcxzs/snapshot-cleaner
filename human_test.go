package main

import (
	"flag"
	"fmt"
	"io"
	"reflect"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{10 * 1024, "10.0 KiB"},
		{100 * 1024, "100 KiB"},
		{575598592, "549 MiB"},
		{1 << 30, "1.00 GiB"},
		{1 << 40, "1.00 TiB"},
	} {
		if got := FormatBytes(tc.in); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{"1024", 1024},
		{"1K", 1024},
		{"50M", 50 * 1024 * 1024},
		{"1.5G", 1536 * 1024 * 1024},
		{"10MiB", 10 * 1024 * 1024},
		{"2GiB", 2 * 1024 * 1024 * 1024},
		{"1MB", 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{" 100M ", 100 * 1024 * 1024},
	} {
		got, err := ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "abc", "10X", "M"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should have failed", bad)
		}
	}
}

func TestParseIDs(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []int
	}{
		{[]string{"1"}, []int{1}},
		{[]string{"1,3"}, []int{1, 3}},
		{[]string{"7-9"}, []int{7, 8, 9}},
		{[]string{"1,3,7-9"}, []int{1, 3, 7, 8, 9}},
		{[]string{"3", "1"}, []int{1, 3}},
		{[]string{"2,2,2"}, []int{2}},
		{[]string{"1-3,2-4"}, []int{1, 2, 3, 4}},
	} {
		got, err := ParseIDs(tc.in)
		if err != nil {
			t.Errorf("ParseIDs(%v) error: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseIDs(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range [][]string{{"x"}, {"5-1"}, {""}, {"1-"}} {
		if _, err := ParseIDs(bad); err == nil {
			t.Errorf("ParseIDs(%v) should have failed", bad)
		}
	}
}

func TestTruncate(t *testing.T) {
	// Truncation keeps the tail, which is the informative end of a path.
	if got := Truncate("/a/very/long/path/to/file.iso", 12); got != ".../file.iso" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("short", 20); got != "short" {
		t.Errorf("Truncate should leave short strings alone, got %q", got)
	}
}

// The standard flag package stops at the first positional argument, so
// "purge 1 --apply" would silently drop --apply and do a dry run instead of
// the removal that was asked for. permuteArgs is what prevents that.
func TestPermuteArgsAllowsFlagsAfterOperands(t *testing.T) {
	newFS := func() (*flag.FlagSet, *bool, *string) {
		fs := flag.NewFlagSet("purge", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		apply := fs.Bool("apply", false, "")
		live := fs.String("live", "", "")
		return fs, apply, live
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flags last", []string{"1,3", "--apply", "--live", "/home"}},
		{"flags first", []string{"--apply", "--live", "/home", "1,3"}},
		{"interleaved", []string{"--apply", "1,3", "--live", "/home"}},
		{"equals form", []string{"1,3", "--apply", "--live=/home"}},
		{"single dash", []string{"1,3", "-apply", "-live", "/home"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, apply, live := newFS()
			if err := fs.Parse(permuteArgs(fs, tc.args)); err != nil {
				t.Fatal(err)
			}
			if !*apply {
				t.Error("--apply was dropped; this would silently turn a removal into a dry run")
			}
			if *live != "/home" {
				t.Errorf("--live = %q, want /home", *live)
			}
			if got := fs.Args(); len(got) != 1 || got[0] != "1,3" {
				t.Errorf("operands = %v, want [1,3]", got)
			}
		})
	}
}

func TestPermuteArgsStopsAtDoubleDash(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "")
	if err := fs.Parse(permuteArgs(fs, []string{"1", "--", "--apply"})); err != nil {
		t.Fatal(err)
	}
	if *apply {
		t.Error("--apply after -- must be treated as an operand, not a flag")
	}
	if got := fs.Args(); len(got) != 2 {
		t.Errorf("operands = %v, want two", got)
	}
}

func TestParseSelectionSplitsFileAndFolderIDs(t *testing.T) {
	sel, err := ParseSelection([]string{"1,3", "F2,F5-F7", "9-10", "f12"})
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	wantFiles := []int{1, 3, 9, 10}
	wantFolders := []int{2, 5, 6, 7, 12}
	if fmt.Sprint(sel.Files) != fmt.Sprint(wantFiles) {
		t.Errorf("Files = %v, want %v", sel.Files, wantFiles)
	}
	if fmt.Sprint(sel.Folders) != fmt.Sprint(wantFolders) {
		t.Errorf("Folders = %v, want %v", sel.Folders, wantFolders)
	}
}

func TestParseSelectionAcceptsAHalfPrefixedRange(t *testing.T) {
	sel, err := ParseSelection([]string{"F5-7"})
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	if fmt.Sprint(sel.Folders) != fmt.Sprint([]int{5, 6, 7}) {
		t.Errorf("Folders = %v, want [5 6 7]", sel.Folders)
	}
	if len(sel.Files) != 0 {
		t.Errorf("Files = %v, want none", sel.Files)
	}
}

// A file id and a folder id name rows in two different tables, so a range
// across them names nothing and must not be guessed at.
func TestParseSelectionRejectsMixedRanges(t *testing.T) {
	for _, bad := range [][]string{{"3-F5"}, {"F"}, {"F3-F1"}, {"Fx"}} {
		if sel, err := ParseSelection(bad); err == nil {
			t.Errorf("ParseSelection(%v) = %+v, want an error", bad, sel)
		}
	}
}

func TestParseIDsRejectsFolderIDs(t *testing.T) {
	if _, err := ParseIDs([]string{"F1"}); err == nil {
		t.Error("ParseIDs must not silently accept a folder id")
	}
}
