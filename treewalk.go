package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Walking a snapshot with readdir + lstat costs one or more syscalls per file.
// Across several snapshots of a large home directory that is millions of
// syscalls and millions of small, scattered metadata reads - which is what
// starves the USB wifi adapter of interrupt servicing (see priority.go).
//
// The subvolume tree already holds every name and every inode, packed into
// leaves. BTRFS_IOC_TREE_SEARCH_V2 - the same ioctl extents.go uses to read
// extent items - hands those leaves over a megabyte at a time. One sweep of the
// tree replaces the entire recursive walk: no path resolution, no per-file
// syscall, and sequential rather than scattered metadata reads.
//
// It needs CAP_SYS_ADMIN, so the readdir walk stays as the unprivileged
// fallback exactly as FIEMAP backs up tree-search measurement.

// btrfs key types used here, from btrfs_tree.h.
const (
	btrfsInodeItemKey   = 1
	btrfsInodeRefKey    = 12
	btrfsInodeExtrefKey = 13
)

// Offsets into the packed on-disk btrfs_inode_item. Named rather than inlined
// for the same reason as the file-extent offsets in extents.go: they are
// checkable against btrfs_tree.h.
const (
	iiSize     = 16  // __le64 size
	iiNlink    = 40  // __le32 nlink
	iiMode     = 52  // __le32 mode
	iiMtimeSec = 136 // atime(112) ctime(124) mtime(136), each __le64 sec + __le32 nsec
	iiMtimeNs  = 144
	iiLen      = 160
)

// Offsets into the packed btrfs_inode_ref / btrfs_inode_extref.
const (
	irNameLen   = 8  // after __le64 index
	irHeadLen   = 10 // index + name_len
	xrParentObj = 0  // __le64 parent_objectid
	xrNameLen   = 16 // after parent_objectid + index
	xrHeadLen   = 18
)

// maxPathDepth bounds parent-chain resolution. A corrupt or looping chain must
// not become an infinite loop in a tool that runs unattended.
const maxPathDepth = 256

// dirRef is a directory's single name and parent.
type dirRef struct {
	parent uint64
	name   string
}

// fileRef is one link to a regular file: a file with two hardlinks in the same
// snapshot occupies one set of extents but appears under two paths, which is
// exactly what the readdir walk reports too.
type fileRef struct {
	ino    uint64
	parent uint64
	name   string
}

// TreeWalkSupported reports whether the tree-search walk can be used here.
// It probes the real thing rather than guessing from the effective uid, since
// what matters is whether the ioctl is permitted.
func TreeWalkSupported(root string) error {
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	defer f.Close()

	argsSize := int(unsafe.Sizeof(btrfsIoctlSearchArgsV2{}))
	buf := make([]byte, argsSize+4096)
	args := (*btrfsIoctlSearchArgsV2)(unsafe.Pointer(&buf[0]))
	*args = btrfsIoctlSearchArgsV2{
		Key: btrfsIoctlSearchKey{
			MinObjectID: btrfsFirstFreeObjectID,
			MaxObjectID: btrfsFirstFreeObjectID,
			MinType:     btrfsInodeItemKey,
			MaxType:     btrfsInodeItemKey,
			MaxOffset:   ^uint64(0),
			MaxTransID:  ^uint64(0),
			NrItems:     1,
		},
		BufSize: 4096,
	}
	if err := ioctl(int(f.Fd()), iocTreeSearchV2, unsafe.Pointer(&buf[0])); err != nil {
		return fmt.Errorf("TREE_SEARCH_V2: %w", err)
	}
	return nil
}

// treeWalkSnapshot sweeps one snapshot's subvolume tree and returns every
// regular file at or above floor, with the path of every link to it.
//
// Only two item types are collected. Directories are kept whatever their size,
// because they are needed to reconstruct paths; regular files are kept only
// above the floor, so memory stays proportional to what the caller can actually
// report rather than to the whole tree.
func treeWalkSnapshot(root string, floor uint64) ([]manifestEntry, error) {
	f, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd := int(f.Fd())

	var (
		dirs      = map[uint64]dirRef{}
		files     []fileRef
		fileMeta  = map[uint64]manifestEntry{} // ino -> size/mtime, path filled later
		curIno    uint64
		curIsDir  bool
		curIsFile bool
		items     int
	)

	argsSize := int(unsafe.Sizeof(btrfsIoctlSearchArgsV2{}))
	buf := make([]byte, argsSize+btrfsSearchBufSize)
	args := (*btrfsIoctlSearchArgsV2)(unsafe.Pointer(&buf[0]))

	minObj, minType, minOff := uint64(btrfsFirstFreeObjectID), uint32(btrfsInodeItemKey), uint64(0)
	for {
		*args = btrfsIoctlSearchArgsV2{
			Key: btrfsIoctlSearchKey{
				TreeID:      0, // the tree containing this fd's inode
				MinObjectID: minObj,
				MaxObjectID: ^uint64(0),
				MinOffset:   minOff,
				MaxOffset:   ^uint64(0),
				MaxTransID:  ^uint64(0),
				MinType:     minType,
				MaxType:     btrfsInodeExtrefKey,
				NrItems:     ^uint32(0),
			},
			BufSize: uint64(btrfsSearchBufSize),
		}
		if err := ioctl(fd, iocTreeSearchV2, unsafe.Pointer(&buf[0])); err != nil {
			return nil, fmt.Errorf("TREE_SEARCH_V2 %s: %w", root, err)
		}
		n := int(args.Key.NrItems)
		if n == 0 {
			break
		}

		body := buf[argsSize:]
		off := 0
		var lastObj, lastOff uint64
		var lastType uint32
		for i := 0; i < n; i++ {
			if off+btrfsIoctlSearchHeaderSz > len(body) {
				break
			}
			hdr := (*btrfsIoctlSearchHeader)(unsafe.Pointer(&body[off]))
			itemLen := int(hdr.Len)
			itemOff := off + btrfsIoctlSearchHeaderSz
			if itemOff+itemLen > len(body) {
				break
			}
			item := body[itemOff : itemOff+itemLen]
			lastObj, lastType, lastOff = hdr.ObjectID, hdr.Type, hdr.Offset
			items++

			switch hdr.Type {
			case btrfsInodeItemKey:
				// Items arrive in key order, so an inode's INODE_ITEM (type 1)
				// always precedes its refs (types 12 and 13). That ordering is
				// what lets this stream rather than buffer the whole tree.
				curIno, curIsDir, curIsFile = hdr.ObjectID, false, false
				ii, ok := parseInodeItem(item)
				if !ok {
					break
				}
				switch ii.mode & unix.S_IFMT {
				case unix.S_IFDIR:
					curIsDir = true
				case unix.S_IFREG:
					if ii.size >= floor {
						curIsFile = true
						fileMeta[hdr.ObjectID] = manifestEntry{
							Ino: hdr.ObjectID, Size: ii.size, MtimeNs: ii.mtimeNs,
						}
					}
				}

			case btrfsInodeRefKey:
				if hdr.ObjectID != curIno || (!curIsDir && !curIsFile) {
					break
				}
				parseInodeRefs(item, func(name string) {
					if curIsDir {
						// A directory has exactly one name; taking the first
						// keeps this deterministic if that ever stops holding.
						if _, seen := dirs[hdr.ObjectID]; !seen {
							dirs[hdr.ObjectID] = dirRef{parent: hdr.Offset, name: name}
						}
						return
					}
					files = append(files, fileRef{ino: hdr.ObjectID, parent: hdr.Offset, name: name})
				})

			case btrfsInodeExtrefKey:
				if hdr.ObjectID != curIno || !curIsFile {
					break
				}
				parseInodeExtrefs(item, func(parent uint64, name string) {
					files = append(files, fileRef{ino: hdr.ObjectID, parent: parent, name: name})
				})
			}
			off = itemOff + itemLen
		}

		// Advance past the last item copied. Items the kernel filtered out are
		// simply re-traversed, which costs nothing measurable and keeps this
		// independent of what the ioctl does to the key on return.
		minObj, minType, minOff = lastObj, lastType, lastOff
		switch {
		case minOff != ^uint64(0):
			minOff++
		case minType != ^uint32(0):
			minType, minOff = minType+1, 0
		case minObj != ^uint64(0):
			minObj, minType, minOff = minObj+1, 0, 0
		default:
			minObj = 0 // exhausted the key space
		}
		if minObj == 0 {
			break
		}
	}

	Debugf("treewalk", "%s: %d item(s) read, %d director(y/ies), %d file link(s) at or above %s",
		root, items, len(dirs), len(files), FormatBytes(floor))

	// Resolve paths once the whole tree is known, memoising directory paths so
	// a deep tree costs one resolution per directory rather than one per file.
	memo := map[uint64]string{btrfsFirstFreeObjectID: ""}
	out := make([]manifestEntry, 0, len(files))
	for _, fr := range files {
		dir, ok := resolveDir(fr.parent, dirs, memo)
		if !ok {
			// A parent outside this tree: the file hangs off something we
			// cannot name, so there is no path to report it under.
			Tracef("treewalk", "%s: skipping ino %d, parent %d unresolvable", root, fr.ino, fr.parent)
			continue
		}
		e := fileMeta[fr.ino]
		e.Rel = filepath.Join(dir, fr.name)
		out = append(out, e)
	}
	return out, nil
}

// inodeItem is the part of a btrfs_inode_item this tool needs.
type inodeItem struct {
	mode    uint32
	size    uint64
	mtimeNs int64
}

// parseInodeItem reads a packed on-disk btrfs_inode_item. It returns false for
// anything too short to be one, rather than reading past it.
func parseInodeItem(item []byte) (inodeItem, bool) {
	if len(item) < iiLen {
		return inodeItem{}, false
	}
	return inodeItem{
		mode: binary.LittleEndian.Uint32(item[iiMode:]),
		size: binary.LittleEndian.Uint64(item[iiSize:]),
		mtimeNs: int64(binary.LittleEndian.Uint64(item[iiMtimeSec:]))*int64(time.Second) +
			int64(binary.LittleEndian.Uint32(item[iiMtimeNs:])),
	}, true
}

// parseInodeRefs yields every name packed into one INODE_REF item. A single
// item holds more than one ref when an inode has several links inside the same
// directory, so this cannot assume one name per item.
func parseInodeRefs(item []byte, yield func(name string)) {
	for p := 0; p+irHeadLen <= len(item); {
		nameLen := int(binary.LittleEndian.Uint16(item[p+irNameLen:]))
		if nameLen == 0 || p+irHeadLen+nameLen > len(item) {
			return
		}
		yield(string(item[p+irHeadLen : p+irHeadLen+nameLen]))
		p += irHeadLen + nameLen
	}
}

// parseInodeExtrefs yields every (parent, name) packed into one INODE_EXTREF
// item. btrfs uses these instead of INODE_REF when a directory holds so many
// links to one inode that the ref item would not fit in a leaf, so each entry
// carries its own parent.
func parseInodeExtrefs(item []byte, yield func(parent uint64, name string)) {
	for p := 0; p+xrHeadLen <= len(item); {
		nameLen := int(binary.LittleEndian.Uint16(item[p+xrNameLen:]))
		if nameLen == 0 || p+xrHeadLen+nameLen > len(item) {
			return
		}
		yield(binary.LittleEndian.Uint64(item[p+xrParentObj:]),
			string(item[p+xrHeadLen:p+xrHeadLen+nameLen]))
		p += xrHeadLen + nameLen
	}
}

// resolveDir walks a directory's parent chain to the subvolume root.
func resolveDir(ino uint64, dirs map[uint64]dirRef, memo map[uint64]string) (string, bool) {
	if p, ok := memo[ino]; ok {
		return p, true
	}
	var chain []uint64
	cur := ino
	for depth := 0; depth < maxPathDepth; depth++ {
		if _, ok := memo[cur]; ok {
			break
		}
		ref, ok := dirs[cur]
		if !ok {
			return "", false
		}
		chain = append(chain, cur)
		cur = ref.parent
	}
	base, ok := memo[cur]
	if !ok {
		return "", false
	}
	// Unwind outermost-first, filling the memo for every directory on the way.
	for i := len(chain) - 1; i >= 0; i-- {
		base = filepath.Join(base, dirs[chain[i]].name)
		memo[chain[i]] = base
	}
	return base, true
}
