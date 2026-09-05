package main

import (
	"bytes"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux ioctl request encoding. Defined here rather than pulled from a
// dependency: x/sys/unix carries no btrfs or FIEMAP definitions, and computing
// the numbers from the struct sizes keeps them verifiable against the kernel
// headers instead of appearing as unexplained magic constants.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirShift | typ<<iocTypeShift | nr<<iocNRShift | size<<iocSizeShift
}

const (
	btrfsIoctlMagic = 0x94
	fiemapMagic     = 'f'

	// BTRFS_SUPER_MAGIC, used to refuse non-btrfs targets early.
	btrfsSuperMagic = 0x9123683E

	// Two distinct read-only flags exist and they are easy to conflate.
	// BTRFS_SUBVOL_RDONLY is the ioctl-level flag used by SUBVOL_GETFLAGS and
	// SUBVOL_SETFLAGS; BTRFS_ROOT_SUBVOL_RDONLY is the on-disk root-item flag,
	// and it is that one which GET_SUBVOL_INFO reports in its flags field.
	// Testing the wrong bit reports every read-only snapshot as writable.
	btrfsSubvolRDOnly     = 1 << 1 // BTRFS_SUBVOL_RDONLY, for SUBVOL_[GS]ETFLAGS
	btrfsRootSubvolRDOnly = 1 << 0 // BTRFS_ROOT_SUBVOL_RDONLY, for GET_SUBVOL_INFO

	// Key types and objectids from btrfs_tree.h.
	btrfsExtentDataKey       = 108
	btrfsFirstFreeObjectID   = 256
	btrfsFileExtentInline    = 0
	btrfsFileExtentReg       = 1
	btrfsFileExtentPrealloc  = 2
	btrfsSearchBufSize       = 1 << 20
	btrfsIoctlSearchHeaderSz = 32
)

// FIEMAP flags we care about.
const (
	fiemapFlagSync                = 0x00000001
	fiemapExtentLast              = 0x00000001
	fiemapExtentUnknown           = 0x00000002
	fiemapExtentDelalloc          = 0x00000004
	fiemapExtentEncoded           = 0x00000008
	fiemapExtentDataInline        = 0x00000200
	fiemapExtentUnwritten         = 0x00000800
	fiemapExtentShared            = 0x00002000
	fiemapMaxOffset        uint64 = ^uint64(0)
)

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type btrfsIoctlSearchKey struct {
	TreeID      uint64
	MinObjectID uint64
	MaxObjectID uint64
	MinOffset   uint64
	MaxOffset   uint64
	MinTransID  uint64
	MaxTransID  uint64
	MinType     uint32
	MaxType     uint32
	NrItems     uint32
	Unused      uint32
	Unused1     uint64
	Unused2     uint64
	Unused3     uint64
	Unused4     uint64
}

type btrfsIoctlSearchHeader struct {
	TransID  uint64
	ObjectID uint64
	Offset   uint64
	Type     uint32
	Len      uint32
}

type btrfsIoctlSearchArgsV2 struct {
	Key     btrfsIoctlSearchKey
	BufSize uint64
	// Buffer follows in the allocation; see treeSearch.
}

type btrfsIoctlInoLookupArgs struct {
	TreeID   uint64
	ObjectID uint64
	Name     [4080]byte
}

type btrfsIoctlTimespec struct {
	Sec      uint64
	Nsec     uint32
	Reserved uint32
}

type btrfsIoctlGetSubvolInfoArgs struct {
	TreeID       uint64
	Name         [256]byte
	ParentID     uint64
	DirID        uint64
	Generation   uint64
	Flags        uint64
	UUID         [16]byte
	ParentUUID   [16]byte
	ReceivedUUID [16]byte
	CTransID     uint64
	OTransID     uint64
	STransID     uint64
	RTransID     uint64
	CTime        btrfsIoctlTimespec
	OTime        btrfsIoctlTimespec
	STime        btrfsIoctlTimespec
	RTime        btrfsIoctlTimespec
	Reserved     [8]uint64
}

// Request numbers, derived from the struct sizes so they stay correct.
var (
	iocSubvolGetflags = ioc(iocRead, btrfsIoctlMagic, 25, 8)
	iocSubvolSetflags = ioc(iocWrite, btrfsIoctlMagic, 26, 8)
	iocTreeSearchV2   = ioc(iocRead|iocWrite, btrfsIoctlMagic, 17, unsafe.Sizeof(btrfsIoctlSearchArgsV2{}))
	iocInoLookup      = ioc(iocRead|iocWrite, btrfsIoctlMagic, 18, unsafe.Sizeof(btrfsIoctlInoLookupArgs{}))
	iocGetSubvolInfo  = ioc(iocRead, btrfsIoctlMagic, 60, unsafe.Sizeof(btrfsIoctlGetSubvolInfoArgs{}))
	iocFiemap         = ioc(iocRead|iocWrite, fiemapMagic, 11, unsafe.Sizeof(fiemapHeader{}))
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// IsBtrfs reports whether path lives on a btrfs filesystem.
func IsBtrfs(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Type == btrfsSuperMagic, nil
}

// FreeBytes returns the filesystem's available bytes for path.
func FreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// SubvolFlags reads the subvolume flags for the subvolume root at path.
func SubvolFlags(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var flags uint64
	if err := ioctl(int(f.Fd()), iocSubvolGetflags, unsafe.Pointer(&flags)); err != nil {
		return 0, fmt.Errorf("SUBVOL_GETFLAGS %s: %w", path, err)
	}
	return flags, nil
}

// SetSubvolFlags writes the subvolume flags for the subvolume root at path.
func SetSubvolFlags(path string, flags uint64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := ioctl(int(f.Fd()), iocSubvolSetflags, unsafe.Pointer(&flags)); err != nil {
		return fmt.Errorf("SUBVOL_SETFLAGS %s: %w", path, err)
	}
	return nil
}

// IsReadOnlySubvol reports whether path is a read-only subvolume.
func IsReadOnlySubvol(path string) (bool, error) {
	flags, err := SubvolFlags(path)
	if err != nil {
		return false, err
	}
	return flags&btrfsSubvolRDOnly != 0, nil
}

// SetSubvolReadOnly toggles just the read-only bit, preserving other flags.
func SetSubvolReadOnly(path string, ro bool) error {
	flags, err := SubvolFlags(path)
	if err != nil {
		return err
	}
	if ro {
		flags |= btrfsSubvolRDOnly
	} else {
		flags &^= btrfsSubvolRDOnly
	}
	return SetSubvolFlags(path, flags)
}

// SubvolInfo describes a subvolume root.
type SubvolInfo struct {
	TreeID       uint64
	ReadOnly     bool
	ReceivedUUID string
	ParentUUID   string
	UUID         string

	// Generation is the transid the subvolume was created at; CTransID is the
	// transid of the last change to its tree. Together with UUID they identify
	// one immutable state of one snapshot, which is what the scan cache is
	// keyed on: any modification of the subvolume moves CTransID, so a cache
	// entry that still matches provably describes unchanged content.
	Generation uint64
	CTransID   uint64
}

func formatUUID(b [16]byte) string {
	if bytes.Equal(b[:], make([]byte, 16)) {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// GetSubvolInfo returns identity and flags for the subvolume root at path.
// Falls back to SUBVOL_GETFLAGS on kernels or permissions where the richer
// ioctl is unavailable, so a missing GET_SUBVOL_INFO never blocks the tool.
func GetSubvolInfo(path string) (*SubvolInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var args btrfsIoctlGetSubvolInfoArgs
	if err := ioctl(int(f.Fd()), iocGetSubvolInfo, unsafe.Pointer(&args)); err == nil {
		return &SubvolInfo{
			TreeID:       args.TreeID,
			ReadOnly:     args.Flags&btrfsRootSubvolRDOnly != 0,
			ReceivedUUID: formatUUID(args.ReceivedUUID),
			ParentUUID:   formatUUID(args.ParentUUID),
			UUID:         formatUUID(args.UUID),
			Generation:   args.Generation,
			CTransID:     args.CTransID,
		}, nil
	}

	// GET_SUBVOL_INFO is unavailable, so UUID and CTransID stay zero. That is
	// deliberate rather than approximated: without them the snapshot has no
	// cache key, and the cache treats it as uncacheable and always walks it.
	// Guessing a key here would risk reusing a manifest for changed content.
	var flags uint64
	if err := ioctl(int(f.Fd()), iocSubvolGetflags, unsafe.Pointer(&flags)); err != nil {
		return nil, fmt.Errorf("not a subvolume root or inaccessible: %s: %w", path, err)
	}
	id, _ := SubvolTreeID(path)
	return &SubvolInfo{TreeID: id, ReadOnly: flags&btrfsSubvolRDOnly != 0}, nil
}

// SubvolTreeID returns the id of the subvolume containing path.
func SubvolTreeID(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	args := btrfsIoctlInoLookupArgs{TreeID: 0, ObjectID: btrfsFirstFreeObjectID}
	if err := ioctl(int(f.Fd()), iocInoLookup, unsafe.Pointer(&args)); err != nil {
		return 0, fmt.Errorf("INO_LOOKUP %s: %w", path, err)
	}
	return args.TreeID, nil
}

// IsSubvolRoot reports whether path is the root of a btrfs subvolume. Subvolume
// roots always carry inode number 256 and are directories.
func IsSubvolRoot(path string) bool {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return false
	}
	return st.Ino == btrfsFirstFreeObjectID && st.Mode&unix.S_IFMT == unix.S_IFDIR
}

type btrfsIoctlFSInfoArgs struct {
	MaxID          uint64
	NumDevices     uint64
	FSID           [16]byte
	Nodesize       uint32
	Sectorsize     uint32
	CloneAlignment uint32
	CsumType       uint16
	CsumSize       uint16
	Flags          uint64
	Generation     uint64
	MetadataUUID   [16]byte
	Reserved       [944]byte
}

var iocFSInfo = ioc(iocRead, btrfsIoctlMagic, 31, unsafe.Sizeof(btrfsIoctlFSInfoArgs{}))

// FilesystemID returns the btrfs filesystem UUID for path. Two paths belong to
// the same filesystem exactly when these match, which is the check that matters
// for extent sharing; comparing device paths or st_dev would be wrong, since
// every subvolume reports a distinct st_dev.
func FilesystemID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var args btrfsIoctlFSInfoArgs
	if err := ioctl(int(f.Fd()), iocFSInfo, unsafe.Pointer(&args)); err != nil {
		return "", fmt.Errorf("FS_INFO %s: %w", path, err)
	}
	return formatUUID(args.FSID), nil
}
