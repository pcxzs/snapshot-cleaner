package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Method records how a usage figure was obtained, so the report can be honest
// about its accuracy instead of presenting every number as equally solid.
type Method string

const (
	MethodTreeSearch Method = "treesearch" // exact on-disk bytes, compression-aware
	MethodFiemap     Method = "fiemap"     // portable; upper bound on compressed data
	MethodStat       Method = "stat"       // last resort, allocated blocks of one copy
	MethodNone       Method = "none"
)

// SetUsage is the answer to "how many bytes does this set of files hold".
type SetUsage struct {
	Bytes      uint64
	Method     Method
	Compressed bool // set when compressed extents were involved
	Exact      bool
}

// Approx reports whether the figure should be shown with a "~" marker.
func (u SetUsage) Approx() bool { return !u.Exact }

// diskExtent is one physical extent, keyed by its address on disk.
type diskExtent struct {
	Addr       uint64 // disk_bytenr, or FIEMAP physical offset
	Bytes      uint64 // disk_num_bytes, or FIEMAP length
	Compressed bool
}

// MeasureSet returns the disk bytes held by a set of file copies, counting each
// physical extent exactly once no matter how many of the copies reference it.
//
// This is the number the tool ranks on. It cannot be obtained from
// "btrfs filesystem du": given N paths that tool prints N independent rows, and
// for a file reflinked across snapshots every row reads Exclusive 0 with the
// full size as "set shared", so the rows sum to zero. The union has to be
// computed directly from the extents.
func MeasureSet(paths []string) SetUsage {
	if len(paths) == 0 {
		return SetUsage{Method: MethodNone}
	}

	// Preferred: read the extent items straight out of the subvolume tree,
	// which yields true on-disk lengths for compressed extents.
	if union, compressed, ok := unionViaTreeSearch(paths); ok {
		Count("measure.treesearch", 1)
		Tracef("measure", "treesearch: %d path(s) -> %s (compressed=%v) first=%s",
			len(paths), FormatBytes(union), compressed, paths[0])
		return SetUsage{Bytes: union, Method: MethodTreeSearch, Compressed: compressed, Exact: true}
	}

	// Portable fallback. FIEMAP reports the logical length of compressed
	// extents, so the total is an upper bound whenever ENCODED extents appear.
	if union, encoded, ok := unionViaFiemap(paths); ok {
		Count("measure.fiemap", 1)
		Tracef("measure", "fiemap: %d path(s) -> %s (encoded=%v) first=%s",
			len(paths), FormatBytes(union), encoded, paths[0])
		return SetUsage{Bytes: union, Method: MethodFiemap, Compressed: encoded, Exact: !encoded}
	}

	// Last resort: allocated blocks of the largest copy. Copies share extents,
	// so the maximum is a far better estimate of the set than the sum.
	var best uint64
	for _, p := range paths {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			continue
		}
		if b := uint64(st.Blocks) * 512; b > best {
			best = b
		}
	}
	if best == 0 {
		Count("measure.failed", 1)
		Warnf("measure", "no method could measure %d path(s), first=%s", len(paths), paths[0])
		return SetUsage{Method: MethodNone}
	}
	Count("measure.stat", 1)
	Tracef("measure", "stat fallback: %d path(s) -> %s first=%s", len(paths), FormatBytes(best), paths[0])
	return SetUsage{Bytes: best, Method: MethodStat}
}

func unionViaTreeSearch(paths []string) (total uint64, compressed bool, ok bool) {
	var all []diskExtent
	for _, p := range paths {
		exts, err := treeSearchExtents(p)
		if err != nil {
			Debugf("measure", "treesearch unavailable for %s: %v (falling back)", p, err)
			return 0, false, false
		}
		Tracef("measure", "treesearch %s: %d extent(s)", p, len(exts))
		all = append(all, exts...)
	}
	total, compressed = unionExtents(all)
	return total, compressed, true
}

func unionViaFiemap(paths []string) (total uint64, encoded bool, ok bool) {
	var all []diskExtent
	for _, p := range paths {
		exts, err := fiemapExtents(p)
		if err != nil {
			Debugf("measure", "fiemap unavailable for %s: %v (falling back)", p, err)
			return 0, false, false
		}
		Tracef("measure", "fiemap %s: %d extent(s)", p, len(exts))
		all = append(all, exts...)
	}
	total, encoded = unionExtents(all)
	return total, encoded, true
}

// Offsets within the packed btrfs_file_extent_item.
const (
	feRamBytes      = 8
	feCompression   = 16
	feType          = 20
	feDiskBytenr    = 21
	feDiskNumBytes  = 29
	feInlineHeadLen = 21
	feRegularLen    = 53
)

// treeSearchExtents reads the EXTENT_DATA items for one file directly from its
// subvolume tree, which is where the true compressed on-disk length lives.
func treeSearchExtents(path string) ([]diskExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd := int(f.Fd())

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}

	lookup := btrfsIoctlInoLookupArgs{ObjectID: btrfsFirstFreeObjectID}
	if err := ioctl(fd, iocInoLookup, unsafe.Pointer(&lookup)); err != nil {
		return nil, fmt.Errorf("INO_LOOKUP: %w", err)
	}

	argsSize := int(unsafe.Sizeof(btrfsIoctlSearchArgsV2{}))
	buf := make([]byte, argsSize+btrfsSearchBufSize)
	args := (*btrfsIoctlSearchArgsV2)(unsafe.Pointer(&buf[0]))

	var out []diskExtent
	var minOffset uint64
	for {
		*args = btrfsIoctlSearchArgsV2{
			Key: btrfsIoctlSearchKey{
				TreeID:      lookup.TreeID,
				MinObjectID: st.Ino,
				MaxObjectID: st.Ino,
				MinOffset:   minOffset,
				MaxOffset:   ^uint64(0),
				MaxTransID:  ^uint64(0),
				MinType:     btrfsExtentDataKey,
				MaxType:     btrfsExtentDataKey,
				NrItems:     ^uint32(0),
			},
			BufSize: uint64(btrfsSearchBufSize),
		}
		if err := ioctl(fd, iocTreeSearchV2, unsafe.Pointer(&buf[0])); err != nil {
			return nil, fmt.Errorf("TREE_SEARCH_V2: %w", err)
		}
		n := int(args.Key.NrItems)
		if n == 0 {
			break
		}

		items := buf[argsSize:]
		off := 0
		var lastOffset uint64
		for i := 0; i < n; i++ {
			if off+btrfsIoctlSearchHeaderSz > len(items) {
				break
			}
			hdr := (*btrfsIoctlSearchHeader)(unsafe.Pointer(&items[off]))
			itemLen := int(hdr.Len)
			itemOff := off + btrfsIoctlSearchHeaderSz
			if itemOff+itemLen > len(items) {
				break
			}
			item := items[itemOff : itemOff+itemLen]
			lastOffset = hdr.Offset

			if hdr.Type == btrfsExtentDataKey && itemLen >= feRegularLen {
				switch item[feType] {
				case btrfsFileExtentReg, btrfsFileExtentPrealloc:
					bytenr := binary.LittleEndian.Uint64(item[feDiskBytenr:])
					numBytes := binary.LittleEndian.Uint64(item[feDiskNumBytes:])
					// A zero disk address is a hole; it occupies nothing.
					if bytenr != 0 {
						out = append(out, diskExtent{
							Addr:       bytenr,
							Bytes:      numBytes,
							Compressed: item[feCompression] != 0,
						})
					}
				}
			}
			off = itemOff + itemLen
		}

		if lastOffset == ^uint64(0) {
			break
		}
		minOffset = lastOffset + 1
	}
	return out, nil
}

// fiemapExtents enumerates a file's physical extents with the portable ioctl.
func fiemapExtents(path string) ([]diskExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd := int(f.Fd())

	const batch = 512
	hdrSize := int(unsafe.Sizeof(fiemapHeader{}))
	extSize := int(unsafe.Sizeof(fiemapExtent{}))
	buf := make([]byte, hdrSize+batch*extSize)
	hdr := (*fiemapHeader)(unsafe.Pointer(&buf[0]))

	var out []diskExtent
	var start uint64
	for {
		*hdr = fiemapHeader{
			Start:       start,
			Length:      fiemapMaxOffset - start,
			Flags:       fiemapFlagSync,
			ExtentCount: batch,
		}
		if err := ioctl(fd, iocFiemap, unsafe.Pointer(&buf[0])); err != nil {
			return nil, fmt.Errorf("FIEMAP: %w", err)
		}
		n := int(hdr.MappedExtents)
		if n == 0 {
			break
		}

		exts := unsafe.Slice((*fiemapExtent)(unsafe.Pointer(&buf[hdrSize])), n)
		last := exts[n-1]
		for _, e := range exts {
			// Inline data lives in metadata and delalloc is not on disk yet;
			// neither is reclaimed by unlinking the file's extents.
			if e.Flags&(fiemapExtentDataInline|fiemapExtentDelalloc|fiemapExtentUnknown) != 0 {
				continue
			}
			out = append(out, diskExtent{
				Addr:       e.Physical,
				Bytes:      e.Length,
				Compressed: e.Flags&fiemapExtentEncoded != 0,
			})
		}

		if last.Flags&fiemapExtentLast != 0 {
			break
		}
		next := last.Logical + last.Length
		if next <= start {
			break
		}
		start = next
	}
	return out, nil
}

// ProbeTreeSearch reports whether the exact extent-accounting path actually
// works here. It exercises the whole path rather than just the lookup ioctl,
// because TREE_SEARCH_V2 needs privileges that INO_LOOKUP does not.
func ProbeTreeSearch(dir string) error {
	f, err := os.CreateTemp(dir, ".sc-probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	// A hole-free block so the file has at least one real extent to report.
	if _, err := f.Write(make([]byte, 8192)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()

	_, err = treeSearchExtents(f.Name())
	return err
}

// unionExtents sums a set of extents, counting each physical extent exactly
// once. This is the whole point of the measurement: a file reflinked into N
// snapshots yields N references to the same extents, and the space freed by
// removing all N copies is the size of the underlying extents, not N times it.
func unionExtents(exts []diskExtent) (total uint64, compressed bool) {
	seen := make(map[uint64]uint64, len(exts))
	for _, e := range exts {
		if e.Compressed {
			compressed = true
		}
		// Two references to the same disk address are the same physical
		// extent; keep the larger length if they disagree on extent size.
		if cur, dup := seen[e.Addr]; !dup || e.Bytes > cur {
			seen[e.Addr] = e.Bytes
		}
	}
	for _, b := range seen {
		total += b
	}
	return total, compressed
}
