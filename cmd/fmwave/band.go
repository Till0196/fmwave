// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package main

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"fmwave/internal/siano"
)

// The band scanned: Japanese FM, 76.0 MHz up to the top of wide FM at 95.0.
const (
	bandLowTenths  = 760
	bandHighTenths = 950
)

// reading is how one frequency came in.
type reading struct {
	MHz  float64 `json:"mhz"`
	Lock int32   `json:"lock"`
	DBm  int32   `json:"dbm"`
	OK   bool    `json:"ok"`
}

// errStopped reports a sweep called off before the end of the band.
var errStopped = errors.New("the sweep was stopped")

// sweep tunes every 0.1 MHz of the band and reads the level, calling each with
// every reading as it comes; each returns false to stop there.
func sweep(chip *siano.Device, bandwidth uint32, each func(done, total int, got reading) bool) ([]reading, error) {
	total := bandHighTenths - bandLowTenths + 1
	readings := make([]reading, 0, total)
	for tenth := bandLowTenths; tenth <= bandHighTenths; tenth++ {
		hz := uint32(tenth) * 100_000
		if err := tune(chip, hz, bandwidth); err != nil {
			return readings, fmt.Errorf("tune %.1f MHz: %w", float64(hz)/1e6, err)
		}
		// The level settles within a few blocks of audio.
		time.Sleep(300 * time.Millisecond)
		chip.DrainData()
		lock, dbm, ok := stats(chip)
		got := reading{MHz: float64(tenth) / 10, Lock: lock, DBm: dbm, OK: ok}
		readings = append(readings, got)
		if each != nil && !each(len(readings), total, got) {
			return readings, errStopped
		}
	}
	return readings, nil
}

// tune sets the chip to a frequency in Hz.
func tune(chip *siano.Device, hz, bandwidth uint32) error {
	_, err := chip.Exchange(siano.MsgRFTuneReq, siano.MsgRFTuneRes, siano.ControlMsgID,
		siano.Words(hz, bandwidth, referenceHz), 5*time.Second)
	return err
}

// station is a peak in a sweep.
type station struct {
	MHz float64 `json:"mhz"`
	DBm int32   `json:"dbm"`
}

// stations picks the peaks out of a sweep. The tuner passes a station a notch
// or two either side of its frequency, so a station is a hump: its frequency
// is the middle of what stands within 2 dB of the top, which falls on the
// station's own 0.1 MHz. A hump has to stand 10 dB over the band's usual level.
//
// A station's hump is narrow: what stays within 6 dB of its top spans well
// under a megahertz. A hump wider than 1.2 MHz is not a station and is passed
// over.
func stations(readings []reading) []station {
	var levels []int
	for _, got := range readings {
		if got.OK {
			levels = append(levels, int(got.DBm))
		}
	}
	if len(levels) == 0 {
		return nil
	}
	sort.Ints(levels)
	floor := int32(levels[len(levels)/2])
	// within reports how far either way from index the readings stay no more
	// than margin below top.
	within := func(index int, top, margin int32) (int, int) {
		low, high := index, index
		for low > 0 && readings[low-1].OK && readings[low-1].DBm >= top-margin {
			low--
		}
		for high < len(readings)-1 && readings[high+1].OK && readings[high+1].DBm >= top-margin {
			high++
		}
		return low, high
	}
	var found []station
	for index := 0; index < len(readings); index++ {
		got := readings[index]
		if !got.OK || got.DBm < floor+10 {
			continue
		}
		// The top of the hump this reading is on.
		top := got.DBm
		low, high := within(index, top, 0)
		for probe := low; probe <= high; probe++ {
			top = max(top, readings[probe].DBm)
		}
		if top != got.DBm {
			continue
		}
		wideLow, wideHigh := within(index, top, 6)
		if wideHigh-wideLow+1 > 12 {
			index = wideHigh
			continue
		}
		nearLow, nearHigh := within(index, top, 2)
		middle := readings[(nearLow+nearHigh)/2]
		found = append(found, station{MHz: middle.MHz, DBm: top})
		index = wideHigh
	}
	return found
}
