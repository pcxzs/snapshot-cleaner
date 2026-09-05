package main

import (
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// buildInodeItem lays out a packed on-disk btrfs_inode_item, which is what the
// offset constants in treewalk.go have to agree with. Writing one here from the
// field order in btrfs_tree.h is the only way to check those offsets without a
// real filesystem, and getting them wrong is the failure mode that would make
// every reported size and mtime silently wrong.
func buildInodeItem(mode uint32, size uint64, mtimeSec uint64, mtimeNsec uint32) []byte {
	b := make([]byte, iiLen)
	put64 := func(off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }
	put32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

	put64(0, 7)     // generation
	put64(8, 8)     // transid
	put64(16, size) // size
	put64(24, size) // nbytes
	put64(32, 0)    // block_group
	put32(40, 1)    // nlink
	put32(44, 1000) // uid
	put32(48, 1000) // gid
	put32(52, mode) // mode
	put64(56, 0)    // rdev
	put64(64, 0)    // flags
	put64(72, 0)    // sequence
	// reserved[4] at 80..112
	put64(112, 1) // atime.sec
	put32(120, 0) // atime.nsec
	put64(124, 2) // ctime.sec
	put32(132, 0) // ctime.nsec
	put64(136, mtimeSec)
	put32(144, mtimeNsec)
	put64(148, 3) // otime.sec
	put32(156, 0) // otime.nsec
	return b
}

func TestParseInodeItemMatchesTheOnDiskLayout(t *testing.T) {
	item := buildInodeItem(unix.S_IFREG|0o644, 4<<30, 1_700_000_000, 123456789)
	got, ok := parseInodeItem(item)
	if !ok {
		t.Fatal("a full-length inode item must parse")
	}
	if got.mode&unix.S_IFMT != unix.S_IFREG {
		t.Errorf("mode: got %#o, want a regular file", got.mode)
	}
	if got.size != 4<<30 {
		t.Errorf("size: got %d, want %d", got.size, uint64(4)<<30)
	}
	if want := int64(1_700_000_000)*1e9 + 123456789; got.mtimeNs != want {
		t.Errorf("mtime: got %d, want %d", got.mtimeNs, want)
	}
}

func TestParseInodeItemRejectsShortItems(t *testing.T) {
	if _, ok := parseInodeItem(make([]byte, iiLen-1)); ok {
		t.Error("a truncated item must be refused, not read past")
	}
}

func TestParseInodeItemRecognisesDirectories(t *testing.T) {
	got, _ := parseInodeItem(buildInodeItem(unix.S_IFDIR|0o755, 4096, 1, 0))
	if got.mode&unix.S_IFMT != unix.S_IFDIR {
		t.Errorf("got mode %#o, want a directory", got.mode)
	}
}

// buildInodeRef packs the given names into one INODE_REF item, as btrfs does
// when an inode has several links inside one directory.
func buildInodeRef(names ...string) []byte {
	var b []byte
	for i, n := range names {
		item := make([]byte, irHeadLen+len(n))
		binary.LittleEndian.PutUint64(item[0:], uint64(i)) // index
		binary.LittleEndian.PutUint16(item[irNameLen:], uint16(len(n)))
		copy(item[irHeadLen:], n)
		b = append(b, item...)
	}
	return b
}

func TestParseInodeRefsReadsEveryPackedName(t *testing.T) {
	var got []string
	parseInodeRefs(buildInodeRef("one.iso", "hardlink.iso"), func(n string) { got = append(got, n) })
	if len(got) != 2 || got[0] != "one.iso" || got[1] != "hardlink.iso" {
		t.Fatalf("got %v, want both packed names", got)
	}
}

func TestParseInodeRefsStopsAtATruncatedName(t *testing.T) {
	item := buildInodeRef("real.iso")
	// Claim a longer name than the item actually holds.
	binary.LittleEndian.PutUint16(item[irNameLen:], 4096)
	var got []string
	parseInodeRefs(item, func(n string) { got = append(got, n) })
	if len(got) != 0 {
		t.Errorf("got %v, want nothing read past the end of the item", got)
	}
}

func buildInodeExtref(parent uint64, name string) []byte {
	b := make([]byte, xrHeadLen+len(name))
	binary.LittleEndian.PutUint64(b[xrParentObj:], parent)
	binary.LittleEndian.PutUint64(b[8:], 1) // index
	binary.LittleEndian.PutUint16(b[xrNameLen:], uint16(len(name)))
	copy(b[xrHeadLen:], name)
	return b
}

func TestParseInodeExtrefsCarriesItsOwnParent(t *testing.T) {
	item := append(buildInodeExtref(300, "a.bin"), buildInodeExtref(301, "b.bin")...)
	type ref struct {
		parent uint64
		name   string
	}
	var got []ref
	parseInodeExtrefs(item, func(p uint64, n string) { got = append(got, ref{p, n}) })
	if len(got) != 2 || got[0] != (ref{300, "a.bin"}) || got[1] != (ref{301, "b.bin"}) {
		t.Fatalf("got %v", got)
	}
}

func TestResolveDirBuildsNestedPaths(t *testing.T) {
	// 256 is the subvolume root; user -> documents -> archive
	dirs := map[uint64]dirRef{
		257: {parent: 256, name: "user"},
		258: {parent: 257, name: "documents"},
		259: {parent: 258, name: "archive"},
	}
	memo := map[uint64]string{btrfsFirstFreeObjectID: ""}

	got, ok := resolveDir(259, dirs, memo)
	if !ok || got != "user/documents/archive" {
		t.Fatalf("got %q (ok=%v), want user/documents/archive", got, ok)
	}
	// Every directory on the way must be memoised, so a deep tree costs one
	// resolution per directory rather than one per file in it.
	if memo[257] != "user" || memo[258] != "user/documents" {
		t.Errorf("intermediate directories not memoised: %v", memo)
	}
	if got, _ := resolveDir(257, dirs, memo); got != "user" {
		t.Errorf("memoised lookup returned %q", got)
	}
}

func TestResolveDirHandlesTheRootItself(t *testing.T) {
	memo := map[uint64]string{btrfsFirstFreeObjectID: ""}
	got, ok := resolveDir(btrfsFirstFreeObjectID, map[uint64]dirRef{}, memo)
	if !ok || got != "" {
		t.Fatalf("got %q (ok=%v), want the empty relative path", got, ok)
	}
}

// A parent that is not in this tree means the file hangs off something we
// cannot name. Reporting it under a wrong path would be worse than omitting it.
func TestResolveDirRefusesAnUnreachableParent(t *testing.T) {
	dirs := map[uint64]dirRef{257: {parent: 999, name: "user"}}
	if _, ok := resolveDir(257, dirs, map[uint64]string{btrfsFirstFreeObjectID: ""}); ok {
		t.Error("a chain that never reaches the root must not resolve")
	}
}

// This tool runs unattended, so a corrupt or looping parent chain must
// terminate rather than hang.
func TestResolveDirTerminatesOnACycle(t *testing.T) {
	dirs := map[uint64]dirRef{
		257: {parent: 258, name: "a"},
		258: {parent: 257, name: "b"},
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := resolveDir(257, dirs, map[uint64]string{btrfsFirstFreeObjectID: ""})
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("a cycle must not resolve to a path")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolveDir did not terminate on a cyclic parent chain")
	}
}
