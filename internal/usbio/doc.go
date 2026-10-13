// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

// Package usbio exposes the raw USB endpoints the stick is driven through; the chip's own
// protocol (internal/siano) sits on top of it.
//
// Transfers reach the hardware through the Go standard library alone, with no cgo, using
// only what each operating system already ships:
//
//	backend  the contract the three below fulfil, and nothing else
//	usbfs    Linux: ioctls on the device nodes under /dev/bus/usb
//	winusb   Windows: SetupAPI to find a device, WinUSB to talk to it
//	iokit    macOS: IOKit, resolved at run time through the Darwin libc bridge
//
// # Unsafe
//
// This package uses none; the backends do, because the three systems' interfaces are C
// ones: kernel-fixed structs, memory outside the Go heap for buffers a kernel holds a
// pointer to, and the function-pointer tables COM and IOKit interfaces are. Values
// arriving in a buffer are decoded with encoding/binary instead.
package usbio
