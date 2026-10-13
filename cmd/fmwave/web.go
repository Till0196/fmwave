// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"fmwave/internal/siano"
)

//go:embed web/index.html
var page []byte

// radio is the chip behind the web page: one tuner, shared by every listener.
type radio struct {
	chip      *siano.Device
	bandwidth uint32

	mu       sync.Mutex
	hz       uint32
	lock     int32
	dbm      int32
	measured bool
	rate     int // bytes a second of audio, the last second
	scanning bool
	scanDone int
	scanOf   int
	found    []station
	readings []reading
	failure  string

	listeners map[chan []byte]struct{}

	// ctx ends when the page is closing; a sweep running then stops, and
	// sweeping is waited for before the chip is let go.
	ctx      context.Context
	sweeping sync.WaitGroup
}

// serve tunes the station asked for, then answers the page, its status and
// the audio until Ctrl-C.
func serve(chip *siano.Device, opts options, listener net.Listener) error {
	station := &radio{chip: chip, bandwidth: opts.bandwidth, listeners: make(map[chan []byte]struct{})}
	start := opts.hz
	if start == 0 {
		start = 80_000_000
	}
	if err := station.tune(start); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if opts.timed && opts.duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, opts.duration)
		defer stop()
	}
	station.ctx = ctx
	defer station.sweeping.Wait()
	go station.pump(ctx)
	go station.watch(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /status", station.status)
	mux.HandleFunc("POST /tune", station.tuneRequest)
	mux.HandleFunc("POST /scan", station.scanRequest)
	mux.HandleFunc("GET /pcm", station.listen)

	fmt.Fprintf(os.Stderr, "listening on http://%s/  (Ctrl-C to stop)\n", listener.Addr())
	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (station *radio) tune(hz uint32) error {
	if err := tune(station.chip, hz, station.bandwidth); err != nil {
		return err
	}
	station.chip.ClearLock()
	station.mu.Lock()
	station.hz = hz
	station.measured = false
	station.mu.Unlock()
	return nil
}

// pump hands every block of audio to whoever is listening, and drops it for a
// listener too far behind rather than holding the chip up: the chip has to be
// read all the time.
func (station *radio) pump(ctx context.Context) {
	var bytes int
	second := time.NewTicker(time.Second)
	defer second.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-second.C:
			station.mu.Lock()
			station.rate = bytes
			station.mu.Unlock()
			bytes = 0
		case block := <-station.chip.Data():
			bytes += len(block)
			station.mu.Lock()
			if station.scanning {
				station.mu.Unlock()
				continue
			}
			for listener := range station.listeners {
				select {
				case listener <- block:
				default:
				}
			}
			station.mu.Unlock()
		}
	}
}

// watch reads the level twice a second while nothing else is tuning.
func (station *radio) watch(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		station.mu.Lock()
		scanning := station.scanning
		station.mu.Unlock()
		if scanning {
			continue
		}
		lock, dbm, ok := stats(station.chip)
		station.mu.Lock()
		if ok {
			station.lock, station.dbm, station.measured = lock, dbm, true
		}
		station.mu.Unlock()
	}
}

func (station *radio) status(w http.ResponseWriter, _ *http.Request) {
	station.mu.Lock()
	defer station.mu.Unlock()
	reply := map[string]any{
		"mhz":      math.Round(float64(station.hz)/1e5) / 10,
		"lock":     station.lock,
		"dbm":      station.dbm,
		"measured": station.measured,
		"rate":     station.rate,
		"scanning": station.scanning,
		"scanDone": station.scanDone,
		"scanOf":   station.scanOf,
		"stations": station.found,
		"sweep":    station.readings,
		"failure":  station.failure,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (station *radio) tuneRequest(w http.ResponseWriter, r *http.Request) {
	mhz, err := strconv.ParseFloat(r.FormValue("mhz"), 64)
	if err != nil || mhz < 76 || mhz > 95 {
		http.Error(w, "mhz must be 76.0-95.0", http.StatusBadRequest)
		return
	}
	station.mu.Lock()
	scanning := station.scanning
	station.mu.Unlock()
	if scanning {
		http.Error(w, "scanning", http.StatusConflict)
		return
	}
	hz := uint32(math.Round(mhz*10)) * 100_000
	if err := station.tune(hz); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scanRequest measures the band in the background and tunes back afterwards.
func (station *radio) scanRequest(w http.ResponseWriter, _ *http.Request) {
	station.mu.Lock()
	if station.scanning {
		station.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	station.scanning = true
	station.scanDone, station.scanOf = 0, bandHighTenths-bandLowTenths+1
	station.readings = nil
	back := station.hz
	station.sweeping.Add(1)
	station.mu.Unlock()
	go func() {
		defer station.sweeping.Done()
		readings, err := sweep(station.chip, station.bandwidth, func(done, total int, got reading) bool {
			station.mu.Lock()
			station.scanDone, station.scanOf = done, total
			station.readings = append(station.readings, got)
			station.mu.Unlock()
			return station.ctx.Err() == nil
		})
		station.mu.Lock()
		switch {
		case errors.Is(err, errStopped):
		case err != nil:
			station.failure = err.Error()
		default:
			station.failure = ""
			station.found = stations(readings)
		}
		station.mu.Unlock()
		_ = station.tune(back)
		station.chip.DrainData()
		station.mu.Lock()
		station.scanning = false
		station.mu.Unlock()
	}()
	w.WriteHeader(http.StatusAccepted)
}

// listen streams the audio as it comes: 16-bit little-endian stereo at 44.1 kHz.
func (station *radio) listen(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	blocks := make(chan []byte, 64)
	station.mu.Lock()
	station.listeners[blocks] = struct{}{}
	station.mu.Unlock()
	defer func() {
		station.mu.Lock()
		delete(station.listeners, blocks)
		station.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Sample-Rate", "44100")
	w.Header().Set("X-Channels", "2")
	for {
		select {
		case <-r.Context().Done():
			return
		case block := <-blocks:
			if _, err := w.Write(block); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
