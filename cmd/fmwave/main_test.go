// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package main

import (
	"encoding/binary"
	"testing"
)

func TestHeaderDescribesStereoPCM(t *testing.T) {
	out := header(options{rate: 44100, channels: 2}, 1000)
	if len(out) != 44 || string(out[:4]) != "RIFF" || string(out[8:16]) != "WAVEfmt " || string(out[36:40]) != "data" {
		t.Fatalf("not a canonical header: %q", out)
	}
	if got := binary.LittleEndian.Uint32(out[28:]); got != 176400 {
		t.Fatalf("byte rate %d, want 176400", got)
	}
	if got := binary.LittleEndian.Uint32(out[4:]); got != 1036 {
		t.Fatalf("RIFF size %d, want 1036", got)
	}
}
