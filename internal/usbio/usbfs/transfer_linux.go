// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

// The pieces of usbfs's transfers with no ioctl in them, kept apart from
// usbfs_linux.go because they are testable on their own.

package usbfs

import "unsafe"

// bufferPointer is the address of a slice's first byte, or nil when it is empty.
func bufferPointer(buffer []byte) unsafe.Pointer {
	if len(buffer) == 0 {
		return nil
	}
	return unsafe.Pointer(&buffer[0])
}
