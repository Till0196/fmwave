// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package usbio

import (
	"fmwave/internal/usbio/backend"
	"fmwave/internal/usbio/iokit"
)

func enumerate() ([]backend.Found, error) { return iokit.Enumerate() }
