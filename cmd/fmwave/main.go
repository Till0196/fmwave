// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

// Command fmwave tunes a Siano Rio stick (MyGica S270, PLEX PX-S1UD) to an FM
// broadcast station and writes what the chip sends, as a WAV file or raw bytes,
// or serves a page to tune and listen on.
//
// The chip is brought up in mode 10 with fm_radio_rio.inp and tuned with
// MSG_SMS_RF_TUNE_REQ carrying the frequency in Hz, a bandwidth code of 0 and
// the 12 MHz reference. The audio arrives the way the television modes'
// transport stream does, as 16-bit stereo PCM at 44.1 kHz, which is what the
// WAV header says unless told otherwise.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fmwave/internal/siano"
	"fmwave/internal/usbio"
)

// The sticks this drives: all the same Rio silicon.
var supported = []usbio.ID{
	{Vendor: 0x187f, Product: 0x0600}, // MyGica S270
	{Vendor: 0x187f, Product: 0x0601}, // MyGica S270
	{Vendor: 0x3275, Product: 0x0080}, // PLEX PX-S1UD
}

// referenceHz is the crystal the tuning request names.
const referenceHz = 12_000_000

// firmwareName is the image the Rio runs for FM.
const firmwareName = "fm_radio_rio.inp"

// Set by the release build.
var (
	version = "devel"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	var (
		megahertz = flag.Float64("f", 0, "frequency in MHz (Japan: 76-95)")
		seconds   = flag.Duration("t", 10*time.Second, "how long to record; 0 runs until Ctrl-C")
		output    = flag.String("o", "fm.wav", "output file; .wav gets a header, anything else is raw, - is stdout")
		firmware  = flag.String("firmware", "", "path to "+firmwareName+" (default: ./firmware/ or next to the executable)")
		selector  = flag.String("device", "", "USB device: empty for the first, bus:address, or an index")
		mode      = flag.Int("mode", siano.ModeFMRadioBDA, "firmware mode: 10, or 9")
		bandwidth = flag.Uint("bandwidth", 0, "bandwidth code in the tune request: 0, or 7 (Linux's BW_FM_RADIO)")
		rate      = flag.Int("rate", 44100, "sample rate the WAV header claims")
		channels  = flag.Int("channels", 2, "channels the WAV header claims")
		verbose   = flag.Bool("v", false, "report signal, message types and the first bytes every second")
		listen    = flag.String("serve", "", "serve a web page to tune and listen on, at this address (e.g. 127.0.0.1:8080)")
		scan      = flag.Bool("scan", false, "measure 76.0-95.0 MHz in 0.1 MHz steps and list the stations")
		showVer   = flag.Bool("version", false, "print version and exit")
		stop      = flag.String("stop", "isdbt", "how to leave the chip quiet: isdbt (switch to the ISDB-T image), zero (tune to 0 Hz), none")
	)
	flag.Parse()
	if *showVer {
		fmt.Printf("%s commit=%s date=%s\n", version, commit, date)
		return
	}
	// -t with -serve is a time limit only when asked for; otherwise the page
	// stays up until Ctrl-C.
	timed := false
	flag.Visit(func(set *flag.Flag) { timed = timed || set.Name == "t" })
	if *megahertz <= 0 && !*scan && *listen == "" {
		fmt.Fprintln(os.Stderr, "usage: fmwave -f 80.0 [-t 10s] [-o fm.wav]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if err := run(options{
		hz:        uint32(*megahertz*1_000_000 + 0.5),
		duration:  *seconds,
		output:    *output,
		firmware:  *firmware,
		selector:  *selector,
		mode:      *mode,
		bandwidth: uint32(*bandwidth),
		rate:      *rate,
		channels:  *channels,
		verbose:   *verbose,
		scan:      *scan,
		serve:     *listen,
		timed:     timed,
		stop:      *stop,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "fmwave:", err)
		os.Exit(1)
	}
}

type options struct {
	hz        uint32
	duration  time.Duration
	output    string
	firmware  string
	selector  string
	mode      int
	bandwidth uint32
	rate      int
	channels  int
	verbose   bool
	scan      bool
	serve     string
	timed     bool
	stop      string
}

func run(opts options) error {
	firmware, err := findFirmware(opts.firmware)
	if err != nil {
		return err
	}
	// The address first: finding it taken is cheaper before the chip has been
	// flashed than after.
	var listener net.Listener
	if opts.serve != "" {
		if listener, err = net.Listen("tcp", opts.serve); err != nil {
			return err
		}
		defer listener.Close()
	}
	chip, err := open(opts.selector)
	if err != nil {
		return err
	}
	defer chip.Close()
	defer rest(chip, opts.stop)
	fmt.Fprintf(os.Stderr, "%s (%04x:%04x)\n", chip.Name(), chip.ID().Vendor, chip.ID().Product)

	if err := chip.SetMode(opts.mode, firmware); err != nil {
		fmt.Fprintln(os.Stderr, "message types seen:", seen(chip))
		return fmt.Errorf("bring the chip up in mode %d: %w", opts.mode, err)
	}
	if version, err := chip.Version(); err == nil {
		fmt.Fprintf(os.Stderr, "firmware mode %d, protocols %#02x\n", version.Mode(), version.SupportedProtocols)
	}
	if opts.scan {
		return scanBand(chip, opts)
	}
	if opts.serve != "" {
		return serve(chip, opts, listener)
	}
	chip.ClearLock()
	chip.DrainData()
	if _, err := chip.Exchange(siano.MsgRFTuneReq, siano.MsgRFTuneRes, siano.ControlMsgID,
		siano.Words(opts.hz, opts.bandwidth, referenceHz), 5*time.Second); err != nil {
		return fmt.Errorf("tune %.1f MHz: %w", float64(opts.hz)/1e6, err)
	}
	fmt.Fprintf(os.Stderr, "tuned %.1f MHz\n", float64(opts.hz)/1e6)

	sink, err := create(opts)
	if err != nil {
		return err
	}
	written, recordErr := record(chip, sink, opts)
	if err := sink.Close(); err != nil && recordErr == nil {
		recordErr = err
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes to %s\n", written, opts.output)
	if written == 0 {
		fmt.Fprintln(os.Stderr, "nothing arrived; message types seen:", seen(chip))
	}
	return recordErr
}

// open claims the stick and starts reading it. The first request of a session
// has been seen to time out once on a chip left in good order, and to go
// through on the next open, so a timeout there is met by opening again.
func open(selector string) (*siano.Device, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		transport, err := usbio.Open(selector, supported)
		if err != nil {
			return nil, err
		}
		chip, err := siano.New(transport)
		if err == nil {
			return chip, nil
		}
		_ = transport.Close()
		if !errors.Is(err, usbio.ErrTimeout) {
			return nil, err
		}
		last = err
		fmt.Fprintln(os.Stderr, "the stick did not answer; opening it again")
	}
	return nil, last
}

// record copies what the chip sends until the time is up or Ctrl-C.
func record(chip *siano.Device, sink io.Writer, opts options) (int64, error) {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	defer signal.Stop(interrupted)

	var deadline <-chan time.Time
	if opts.duration > 0 {
		timer := time.NewTimer(opts.duration)
		defer timer.Stop()
		deadline = timer.C
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	began := time.Now()
	// FM streams from the tune on. If nothing arrives, ask for the stream the
	// way the television modes do, as a fallback.
	asked := false
	var written, lastSecond int64
	var first []byte
	for {
		select {
		case block := <-chip.Data():
			if first == nil {
				first = append([]byte(nil), block[:min(len(block), 32)]...)
			}
			count, err := sink.Write(block)
			written += int64(count)
			if err != nil {
				return written, err
			}
		case <-tick.C:
			if written == 0 && !asked && time.Since(began) > 2*time.Second {
				asked = true
				fmt.Fprintln(os.Stderr, "nothing yet; asking for the whole stream (PID 0x2000)")
				_ = chip.Send(siano.MsgAddPIDFilterReq, siano.ControlMsgID, siano.Words(siano.PIDAll))
			}
			if opts.verbose {
				fmt.Fprintf(os.Stderr, "%4.0fs  %7d B/s  %s  seen %s\n",
					time.Since(began).Seconds(), written-lastSecond, level(chip), seen(chip))
				if first != nil {
					fmt.Fprintf(os.Stderr, "      first bytes %s\n", hex.EncodeToString(first))
					first = []byte{}
				}
			}
			lastSecond = written
		case <-deadline:
			return written, nil
		case <-interrupted:
			return written, nil
		}
	}
}

// rest leaves the chip sending nothing before the handle is let go.
//
// The FM image sends audio for as long as it runs, and a chip that nobody
// reads stops taking requests -- down to its control transfers -- until it is
// unplugged. Tuning away, powering down and the like do not stop it. So the
// chip is left in the ISDB-T image, which sends nothing without a filter, and
// the next open starts from there.
func rest(chip *siano.Device, how string) {
	switch how {
	case "none":
		return
	case "zero":
		_, _ = chip.Exchange(siano.MsgRFTuneReq, siano.MsgRFTuneRes, siano.ControlMsgID,
			siano.Words(0, 0, referenceHz), time.Second)
	default:
		isdbt, err := findImage("isdbt_rio.inp", "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot leave the chip quiet:", err)
			return
		}
		if err := sameRelease(isdbt); err != nil {
			fmt.Fprintln(os.Stderr, "not switching to ISDB-T:", err)
			return
		}
		if err := chip.SetMode(siano.ModeISDBTBDA, isdbt); err != nil {
			fmt.Fprintln(os.Stderr, "cannot leave the chip quiet:", err)
			return
		}
	}
	// Say whether it worked: anything still arriving a moment later is what
	// would pile up once nobody reads.
	chip.DrainData()
	time.Sleep(200 * time.Millisecond)
	chip.DrainData()
	before := chip.Seen()[siano.MsgDVBTBDAData]
	time.Sleep(time.Second)
	if after := chip.Seen()[siano.MsgDVBTBDAData]; after != before {
		fmt.Fprintf(os.Stderr, "still streaming after -stop %s (%d blocks in a second)\n", how, after-before)
	} else {
		fmt.Fprintf(os.Stderr, "quiet after -stop %s\n", how)
	}
}

// sameRelease checks that the ISDB-T image loads where the FM one does.
//
// A running image is replaced by loading the new one at the address it names
// (a word 20 bytes into its payload). The images of one release agree on it --
// the fm_radio_rio.inp and isdbt_rio.inp of the S270 driver both say 0xe20000
// -- but another release's isdbt_rio.inp says 0xe00000, and loading that over
// the FM image stopped the chip 24 KB in, for good, every time.
func sameRelease(isdbt string) error {
	fm, err := findFirmware("")
	if err != nil {
		return err
	}
	address := func(path string) (uint32, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		image, err := siano.ParseFirmware(data)
		if err != nil || len(image.Payload) < 24 {
			return 0, fmt.Errorf("%s: not a firmware image", path)
		}
		return binary.LittleEndian.Uint32(image.Payload[20:]), nil
	}
	fromFM, err := address(fm)
	if err != nil {
		return err
	}
	fromISDBT, err := address(isdbt)
	if err != nil {
		return err
	}
	if fromFM != fromISDBT {
		return fmt.Errorf("%s loads at %#x but %s at %#x; they are from different releases", isdbt, fromISDBT, fm, fromFM)
	}
	return nil
}

// scanBand tunes every 0.1 MHz of the Japanese band (wide FM included) and says
// how each came in, then which look like stations.
func scanBand(chip *siano.Device, opts options) error {
	readings, err := sweep(chip, opts.bandwidth, func(_, _ int, got reading) bool {
		if !got.OK {
			fmt.Printf("%5.1f  (no statistics)\n", got.MHz)
			return true
		}
		bar := strings.Repeat("#", max(0, int(got.DBm+100)/2))
		fmt.Printf("%5.1f  lock %d  %4d dBm  %s\n", got.MHz, got.Lock, got.DBm, bar)
		return true
	})
	if err != nil {
		return err
	}
	for _, found := range stations(readings) {
		fmt.Printf("station %5.1f MHz  %d dBm\n", found.MHz, found.DBm)
	}
	return nil
}

// stats reads the lock word and the level out of the FM statistics.
func stats(chip *siano.Device) (int32, int32, bool) {
	body, err := chip.Exchange(siano.MsgGetStatisticsExReq, siano.MsgGetStatisticsExRes,
		siano.ControlMsgID, nil, time.Second)
	if err != nil || len(body) < 56 {
		return 0, 0, false
	}
	return int32(binary.LittleEndian.Uint32(body[52:])), int32(binary.LittleEndian.Uint32(body[28:])), true
}

// level asks for the statistics and picks out what the FM modes report: the
// mode, a lock word and the received level in dBm.
func level(chip *siano.Device) string {
	body, err := chip.Exchange(siano.MsgGetStatisticsExReq, siano.MsgGetStatisticsExRes,
		siano.ControlMsgID, nil, time.Second)
	if err != nil {
		return fmt.Sprintf("locked %v", chip.Locked())
	}
	word := func(offset int) int32 {
		if len(body) < offset+4 {
			return 0
		}
		return int32(binary.LittleEndian.Uint32(body[offset:]))
	}
	return fmt.Sprintf("mode %d lock %d level %d dBm", word(4), word(52), word(28))
}

// seen lists the message types that arrived, with their counts.
func seen(chip *siano.Device) string {
	counts := chip.Seen()
	types := make([]int, 0, len(counts))
	for messageType := range counts {
		types = append(types, int(messageType))
	}
	sort.Ints(types)
	parts := make([]string, 0, len(types))
	for _, messageType := range types {
		parts = append(parts, fmt.Sprintf("%d×%d", messageType, counts[uint16(messageType)]))
	}
	return strings.Join(parts, " ")
}

// findFirmware looks where it was told, then in ./firmware, the current
// directory and next to the executable.
func findFirmware(given string) (string, error) { return findImage(firmwareName, given) }

func findImage(firmwareName, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	candidates := []string{filepath.Join("firmware", firmwareName), firmwareName}
	if executable, err := os.Executable(); err == nil {
		folder := filepath.Dir(executable)
		candidates = append(candidates, filepath.Join(folder, "firmware", firmwareName), filepath.Join(folder, firmwareName))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found (looked in %s); pass -firmware", firmwareName, strings.Join(candidates, ", "))
}

// output is where the bytes go, with a WAV header in front when the name
// asks for one.
type output struct {
	file   *os.File
	wav    bool
	opts   options
	length int64
}

func create(opts options) (*output, error) {
	if opts.output == "-" {
		return &output{file: os.Stdout, opts: opts}, nil
	}
	file, err := os.Create(opts.output)
	if err != nil {
		return nil, err
	}
	sink := &output{file: file, wav: strings.EqualFold(filepath.Ext(opts.output), ".wav"), opts: opts}
	if sink.wav {
		// The sizes are filled in on Close; until then they say "unknown".
		if _, err := file.Write(header(opts, 0xffffffff-36)); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return sink, nil
}

func (sink *output) Write(data []byte) (int, error) {
	count, err := sink.file.Write(data)
	sink.length += int64(count)
	return count, err
}

func (sink *output) Close() error {
	if sink.file == os.Stdout {
		return nil
	}
	if sink.wav {
		if _, err := sink.file.Seek(0, io.SeekStart); err == nil {
			if _, err := sink.file.Write(header(sink.opts, uint32(min(sink.length, 0xffffffff-36)))); err != nil {
				_ = sink.file.Close()
				return err
			}
		}
	}
	return sink.file.Close()
}

// header is a canonical 44-byte RIFF/WAVE header for 16-bit PCM.
func header(opts options, dataBytes uint32) []byte {
	const bits = 16
	align := opts.channels * bits / 8
	out := make([]byte, 44)
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], 36+dataBytes)
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], uint16(opts.channels))
	binary.LittleEndian.PutUint32(out[24:], uint32(opts.rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(opts.rate*align))
	binary.LittleEndian.PutUint16(out[32:], uint16(align))
	binary.LittleEndian.PutUint16(out[34:], bits)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], dataBytes)
	return out
}
