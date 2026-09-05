package main

import "testing"

func TestUnionExtentsCountsSharedExtentsOnce(t *testing.T) {
	// The same three extents, as they would appear when one file is reflinked
	// into four snapshots. The union must be the size of the extents, not four
	// times it - this is the entire basis of the reclaim figure.
	one := []diskExtent{
		{Addr: 4096, Bytes: 1 << 20},
		{Addr: 1<<20 + 4096, Bytes: 1 << 20},
		{Addr: 2<<20 + 4096, Bytes: 512 << 10},
	}
	var four []diskExtent
	for i := 0; i < 4; i++ {
		four = append(four, one...)
	}

	wantSingle := uint64(1<<20 + 1<<20 + 512<<10)
	if got, _ := unionExtents(one); got != wantSingle {
		t.Fatalf("single copy = %d, want %d", got, wantSingle)
	}
	got, _ := unionExtents(four)
	if got != wantSingle {
		t.Fatalf("four reflinked copies = %d, want %d (shared extents must count once)", got, wantSingle)
	}
}

func TestUnionExtentsDistinctExtentsAdd(t *testing.T) {
	exts := []diskExtent{
		{Addr: 4096, Bytes: 1000},
		{Addr: 8192, Bytes: 2000},
	}
	if got, _ := unionExtents(exts); got != 3000 {
		t.Fatalf("distinct extents = %d, want 3000", got)
	}
}

func TestUnionExtentsFlagsCompression(t *testing.T) {
	if _, c := unionExtents([]diskExtent{{Addr: 1, Bytes: 10}}); c {
		t.Error("should not report compression when no extent is compressed")
	}
	if _, c := unionExtents([]diskExtent{{Addr: 1, Bytes: 10}, {Addr: 2, Bytes: 10, Compressed: true}}); !c {
		t.Error("should report compression when any extent is compressed")
	}
}

func TestUnionExtentsPrefersLargerLengthOnConflict(t *testing.T) {
	// Two references to one address that disagree on length: take the larger,
	// so the figure is never an undercount of what the extent occupies.
	got, _ := unionExtents([]diskExtent{{Addr: 100, Bytes: 4096}, {Addr: 100, Bytes: 8192}})
	if got != 8192 {
		t.Fatalf("got %d, want 8192", got)
	}
}

func TestUnionExtentsEmpty(t *testing.T) {
	if got, _ := unionExtents(nil); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}
