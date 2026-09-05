package main

import (
	"testing"
	"unsafe"
)

// The ioctl request numbers and struct layouts are a kernel ABI that this tool
// declares by hand, because x/sys/unix carries no btrfs or FIEMAP definitions.
// Getting one wrong produces silently wrong answers rather than a build error,
// so they are pinned here against the values the kernel headers actually
// produce. Regenerate with a C program including <linux/btrfs.h>:
//
//	printf("%#lx", (unsigned long)BTRFS_IOC_GET_SUBVOL_INFO);
func TestIoctlRequestNumbersMatchKernelABI(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"BTRFS_IOC_SUBVOL_GETFLAGS", iocSubvolGetflags, 0x80089419},
		{"BTRFS_IOC_SUBVOL_SETFLAGS", iocSubvolSetflags, 0x4008941a},
		{"BTRFS_IOC_TREE_SEARCH_V2", iocTreeSearchV2, 0xc0709411},
		{"BTRFS_IOC_INO_LOOKUP", iocInoLookup, 0xd0009412},
		{"BTRFS_IOC_GET_SUBVOL_INFO", iocGetSubvolInfo, 0x81f8943c},
		{"BTRFS_IOC_FS_INFO", iocFSInfo, 0x8400941f},
		{"FS_IOC_FIEMAP", iocFiemap, 0xc020660b},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#x, want %#x", tc.name, tc.got, tc.want)
		}
	}
}

func TestStructSizesMatchKernelABI(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"btrfs_ioctl_search_args_v2", unsafe.Sizeof(btrfsIoctlSearchArgsV2{}), 112},
		{"btrfs_ioctl_search_key", unsafe.Sizeof(btrfsIoctlSearchKey{}), 104},
		{"btrfs_ioctl_search_header", unsafe.Sizeof(btrfsIoctlSearchHeader{}), 32},
		{"btrfs_ioctl_ino_lookup_args", unsafe.Sizeof(btrfsIoctlInoLookupArgs{}), 4096},
		{"btrfs_ioctl_get_subvol_info_args", unsafe.Sizeof(btrfsIoctlGetSubvolInfoArgs{}), 504},
		{"btrfs_ioctl_fs_info_args", unsafe.Sizeof(btrfsIoctlFSInfoArgs{}), 1024},
		{"fiemap", unsafe.Sizeof(fiemapHeader{}), 32},
		{"fiemap_extent", unsafe.Sizeof(fiemapExtent{}), 56},
	} {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// The search header must be read at the right offset or every extent item
	// is parsed from the wrong bytes.
	if btrfsIoctlSearchHeaderSz != int(unsafe.Sizeof(btrfsIoctlSearchHeader{})) {
		t.Errorf("btrfsIoctlSearchHeaderSz = %d, want %d",
			btrfsIoctlSearchHeaderSz, unsafe.Sizeof(btrfsIoctlSearchHeader{}))
	}
}

func TestGetSubvolInfoFieldOffsets(t *testing.T) {
	var a btrfsIoctlGetSubvolInfoArgs
	base := uintptr(unsafe.Pointer(&a))
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"flags", uintptr(unsafe.Pointer(&a.Flags)) - base, 288},
		{"uuid", uintptr(unsafe.Pointer(&a.UUID)) - base, 296},
		{"received_uuid", uintptr(unsafe.Pointer(&a.ReceivedUUID)) - base, 328},
	} {
		if tc.got != tc.want {
			t.Errorf("offsetof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// GET_SUBVOL_INFO reports the on-disk root-item flag (bit 0), while the
// SUBVOL_GETFLAGS/SETFLAGS ioctls use a different flag (bit 1). Conflating
// them makes every read-only snapshot look writable.
func TestReadOnlyFlagsAreDistinct(t *testing.T) {
	if btrfsSubvolRDOnly != 2 {
		t.Errorf("BTRFS_SUBVOL_RDONLY = %d, want 2", btrfsSubvolRDOnly)
	}
	if btrfsRootSubvolRDOnly != 1 {
		t.Errorf("BTRFS_ROOT_SUBVOL_RDONLY = %d, want 1", btrfsRootSubvolRDOnly)
	}
	if btrfsSubvolRDOnly == btrfsRootSubvolRDOnly {
		t.Fatal("the two read-only flags must not be the same bit")
	}
}
