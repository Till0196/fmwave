// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package usbfs

import (
	"testing"
	"unsafe"
)

// wide is whether this is a 64-bit build. The kernel's usbfs structs hold
// pointers, so their sizes, the offsets after the first pointer and the ioctl
// numbers built from the sizes all differ between a 32-bit kernel and a 64-bit
// one; the values below are <linux/usbdevice_fs.h> on each.
const wide = unsafe.Sizeof(uintptr(0)) == 8

// abi picks the 64-bit or the 32-bit value.
func abi(on64, on32 uintptr) uintptr {
	if wide {
		return on64
	}
	return on32
}

// A drifting field is not a compile error but a request the kernel refuses,
// which nothing short of a device attached would show.
func TestStructsMatchTheKernelABI(t *testing.T) {
	var request urb
	var control controlTransfer
	var bulk bulkTransfer
	for _, one := range []struct {
		name      string
		got, want uintptr
	}{
		{"sizeof(usbdevfs_ctrltransfer)", unsafe.Sizeof(control), abi(24, 16)},
		{"sizeof(usbdevfs_bulktransfer)", unsafe.Sizeof(bulk), abi(24, 16)},
		{"sizeof(usbdevfs_urb)", unsafe.Sizeof(request), abi(56, 44)},
		{"usbdevfs_ctrltransfer.timeout", unsafe.Offsetof(control.Timeout), 8},
		{"usbdevfs_ctrltransfer.data", unsafe.Offsetof(control.Data), abi(16, 12)},
		{"usbdevfs_bulktransfer.data", unsafe.Offsetof(bulk.Data), abi(16, 12)},
		{"usbdevfs_urb.status", unsafe.Offsetof(request.Status), 4},
		{"usbdevfs_urb.flags", unsafe.Offsetof(request.Flags), 8},
		{"usbdevfs_urb.buffer", unsafe.Offsetof(request.Buffer), abi(16, 12)},
		{"usbdevfs_urb.buffer_length", unsafe.Offsetof(request.BufferLength), abi(24, 16)},
		{"usbdevfs_urb.actual_length", unsafe.Offsetof(request.ActualLength), abi(28, 20)},
		{"usbdevfs_urb.signr", unsafe.Offsetof(request.Signal), abi(44, 36)},
		{"usbdevfs_urb.usercontext", unsafe.Offsetof(request.UserContext), abi(48, 40)},
	} {
		if one.got != one.want {
			t.Errorf("%s = %d, want %d", one.name, one.got, one.want)
		}
	}
}

func TestIoctlNumbers(t *testing.T) {
	for _, one := range []struct {
		name      string
		got, want uintptr
	}{
		{"USBDEVFS_CONTROL", usbfsControl, abi(0xC0185500, 0xC0105500)},
		{"USBDEVFS_BULK", usbfsBulk, abi(0xC0185502, 0xC0105502)},
		{"USBDEVFS_SUBMITURB", usbfsSubmitURB, abi(0x8038550A, 0x802C550A)},
		{"USBDEVFS_DISCARDURB", usbfsDiscardURB, 0x0000550B},
		{"USBDEVFS_REAPURBNDELAY", usbfsReapURBNonBlock, abi(0x4008550D, 0x4004550D)},
		{"USBDEVFS_CLAIMINTERFACE", usbfsClaimInterface, 0x8004550F},
		{"USBDEVFS_RELEASEINTERFACE", usbfsReleaseInterface, 0x80045510},
		{"USBDEVFS_CLEAR_HALT", usbfsClearHalt, 0x80045515},
	} {
		if one.got != one.want {
			t.Errorf("%s = %#08x, want %#08x", one.name, one.got, one.want)
		}
	}
}
