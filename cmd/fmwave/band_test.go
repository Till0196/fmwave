// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package main

import "testing"

// Four stations, each a hump a few tenths of a megahertz wide, and one hump
// from 90.8 to 94.9 MHz too wide to be a station.
func TestStationsAreNarrowHumps(t *testing.T) {
	levels := map[int]int32{
		797: -81, 798: -73, 799: -67, 800: -67, 801: -67, 802: -83, 803: -81,
		811: -73, 812: -67, 813: -67, 814: -67, 815: -73,
		830: -72, 831: -67, 832: -65, 833: -67, 834: -73,
		896: -68, 897: -68, 898: -67, 899: -74, 900: -77,
	}
	var readings []reading
	for tenth := bandLowTenths; tenth <= bandHighTenths; tenth++ {
		dbm, ok := levels[tenth]
		switch {
		case ok:
		case tenth >= 908 && tenth <= 948:
			dbm = -53
		default:
			dbm = -88
		}
		readings = append(readings, reading{MHz: float64(tenth) / 10, DBm: dbm, OK: true})
	}
	got := stations(readings)
	want := []float64{80.0, 81.3, 83.2, 89.7}
	if len(got) != len(want) {
		t.Fatalf("stations %v, want %v", got, want)
	}
	for index, found := range got {
		if found.MHz != want[index] {
			t.Fatalf("stations %v, want %v", got, want)
		}
	}
}
