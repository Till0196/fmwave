// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package usbio

import (
	"fmwave/internal/usbio/backend"
	"fmwave/internal/usbio/injected"
	"fmwave/internal/usbio/usbfs"
)

// enumerate lists what usbfs can see plus anything handed to this process from
// outside; handed-in devices come first, being the ones somebody meant. An
// unreadable bus stops being an error once something has been handed in, since
// Android fails every listing yet may already hold a tuner.
func enumerate() ([]backend.Found, error) {
	handed := injected.Default.Found(usbfs.Adopt)
	devices, err := usbfs.Enumerate()
	if len(handed) == 0 {
		return devices, err
	}
	return append(handed, devices...), nil
}
