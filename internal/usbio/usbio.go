// Copyright 2026 Till0196
// SPDX-License-Identifier: GPL-2.0-or-later

package usbio

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"fmwave/internal/usbio/backend"
)

// The errors a caller sees, re-exported from the backend package.
var (
	ErrTimeout     = backend.ErrTimeout
	ErrUnsupported = backend.ErrUnsupported
	ErrGone        = backend.ErrGone
	ErrNoDevices   = backend.ErrNoDevices
)

var errClosed = errors.New("USB device is closed")

// ID is a USB vendor/product pair.
type ID = backend.ID

// IDs builds an ID list from one vendor number and the product numbers keying
// its model map, so the two cannot drift apart.
func IDs[Model any](vendor uint16, models map[uint16]Model) []ID {
	ids := make([]ID, 0, len(models))
	for product := range models {
		ids = append(ids, ID{Vendor: vendor, Product: product})
	}
	return ids
}

// Transport is an opened USB device: its raw control and bulk endpoints.
// Transfers may run concurrently; a protocol needing an atomic sequence
// serialises it itself.
type Transport struct {
	device    backend.Device
	name      string
	vendorID  uint16
	productID uint16
	closed    bool
	// mu guards the closed flag only, not the transfers: serialising those would
	// stop a multi-tuner board working in parallel. Close takes the write lock so
	// it cannot run while a transfer is in flight.
	mu sync.RWMutex
}

// Open claims interface 0 of the first device matching one of the supported IDs.
// The selector is empty for "any", a "bus:address" string, or a match index.
func Open(selector string, supported []ID) (*Transport, error) {
	devices, err := enumerate()
	if err != nil {
		return nil, err
	}
	defer backend.Drop(devices)
	matchedIndex := 0
	for _, candidate := range devices {
		if !candidate.ID.In(supported) {
			continue
		}
		if !selected(selector, candidate.Location, matchedIndex) {
			matchedIndex++
			continue
		}
		opened, err := candidate.Open()
		if err != nil {
			return nil, fmt.Errorf("open USB device %s: %w", candidate.Location, err)
		}
		return &Transport{
			device:    opened,
			name:      candidate.Location,
			vendorID:  candidate.ID.Vendor,
			productID: candidate.ID.Product,
		}, nil
	}
	if selector == "" {
		return nil, errors.New("no supported USB device found")
	}
	return nil, fmt.Errorf("USB device %q was not found or is not supported", selector)
}

// Attached describes a USB device as reported without opening it.
type Attached struct {
	ID ID
	// Location is the "bus:address" name, which doubles as the selector.
	Location string
	// Serial is the device's serial number when the operating system could report
	// it unopened: Linux (sysfs) and macOS (the IOKit registry) know it, Windows
	// needs an open handle for a string descriptor and so leaves it empty.
	Serial string
	// Port is the physical path from the root hub, "1-2.3". Unlike bus and
	// address it survives a re-enumeration and changes only when the device is
	// moved to another socket, so a stable name falls back to it without a serial.
	Port string
}

// Devices lists every USB device present. Nothing is opened and no interface is
// claimed, so this is safe against devices owned by other drivers.
func Devices() ([]Attached, error) {
	devices, err := enumerate()
	if err != nil {
		return nil, err
	}
	attached := make([]Attached, 0, len(devices))
	for _, candidate := range devices {
		if candidate.Release != nil {
			candidate.Release()
		}
		attached = append(attached, Attached{
			ID:       candidate.ID,
			Location: candidate.Location,
			Serial:   candidate.Serial,
			Port:     candidate.Port,
		})
	}
	return attached, nil
}

// Name reports the USB bus:address the device was found at.
func (transport *Transport) Name() string { return transport.name }

// ProductID reports the USB product number the device enumerated with.
func (transport *Transport) ProductID() uint16 { return transport.productID }

// VendorID reports the USB vendor number the device enumerated with.
func (transport *Transport) VendorID() uint16 { return transport.vendorID }

// ID reports the vendor and product pair the device enumerated with, for boards
// that share product numbers across vendors.
func (transport *Transport) ID() ID {
	return ID{Vendor: transport.vendorID, Product: transport.productID}
}

// Descriptor requests, as chapter 9 of the USB specification numbers them.
const (
	requestGetDescriptor  = 0x06
	descriptorTypeDevice  = 0x01
	descriptorTypeString  = 0x03
	descriptorTimeoutMS   = 500
	descriptorTypeShift   = 8
	serialIndexOffset     = 16
	deviceDescriptorBytes = 18
	// languageEnglishUS is the only language a tuner is worth asking for.
	languageEnglishUS = 0x0409
)

// Serial reads the device's serial-number string, empty when it carries none.
// An enclosure enumerating as several devices numbers them within one serial.
func (transport *Transport) Serial() (string, error) {
	descriptor := make([]byte, deviceDescriptorBytes)
	if _, err := transport.getDescriptor(descriptorTypeDevice, 0, 0, descriptor); err != nil {
		return "", fmt.Errorf("read the USB device descriptor: %w", err)
	}
	index := descriptor[serialIndexOffset]
	if index == 0 {
		return "", nil
	}
	// The first two bytes are the descriptor's own length and type; the rest
	// is UTF-16, which for a serial number is always plain ASCII in practice.
	buffer := make([]byte, 255)
	read, err := transport.getDescriptor(descriptorTypeString, index, languageEnglishUS, buffer)
	if err != nil {
		return "", fmt.Errorf("read the USB serial number: %w", err)
	}
	if read < 2 {
		return "", nil
	}
	if length := int(buffer[0]); length < read {
		read = length
	}
	var serial []rune
	for offset := 2; offset+1 < read; offset += 2 {
		serial = append(serial, rune(uint16(buffer[offset])|uint16(buffer[offset+1])<<8))
	}
	return strings.TrimSpace(string(serial)), nil
}

func (transport *Transport) getDescriptor(kind, index byte, language uint16, buffer []byte) (int, error) {
	value := uint16(kind)<<descriptorTypeShift | uint16(index)
	return transport.Control(0x80, requestGetDescriptor, value, language, buffer, descriptorTimeoutMS)
}

// checkOpen reports errClosed once Close has run. Every transfer method calls it
// under the read lock, so a transfer can never race the close.
func (transport *Transport) checkOpen() error {
	if transport.closed {
		return errClosed
	}
	return nil
}

// Control runs a transfer on the default control pipe.
func (transport *Transport) Control(requestType, request byte, value, index uint16, buffer []byte, timeoutMS uint32) (int, error) {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if err := transport.checkOpen(); err != nil {
		return 0, err
	}
	return transport.device.Control(requestType, request, value, index, buffer, timeoutMS)
}

// Bulk runs a transfer on a bulk endpoint, whose direction the address decides.
func (transport *Transport) Bulk(endpoint byte, buffer []byte, timeoutMS uint32) (int, error) {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if err := transport.checkOpen(); err != nil {
		return 0, err
	}
	return transport.device.Bulk(endpoint, buffer, timeoutMS)
}

// Stream is a running endpoint that keeps several transfers outstanding, so it
// stays serviced while the reader is away with the data it last took.
//
// A bulk endpoint is flow-controlled, but the device behind it is not: it fills
// a FIFO of a few kilobytes at the rate its stream arrives, and the host
// collects it only while a transfer is outstanding. Reading one transfer at a
// time leaves a gap after every read, and whatever the FIFO cannot hold in that
// gap is lost.
type Stream struct {
	transport *Transport
	inner     backend.Stream
	closed    bool
	mu        sync.Mutex
}

// OpenBulkStream starts reading a bulk endpoint with depth transfers of size
// bytes outstanding, returning ErrUnsupported where the backend has no ring.
// The caller closes the stream; reads take a buffer of at least size bytes.
func (transport *Transport) OpenBulkStream(endpoint byte, size, depth int, timeoutMS uint32) (*Stream, error) {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if err := transport.checkOpen(); err != nil {
		return nil, err
	}
	if size <= 0 || depth <= 0 || endpoint&0x80 == 0 {
		return nil, errors.New("invalid bulk stream")
	}
	inner, err := transport.device.OpenBulkStream(endpoint, size, depth, timeoutMS)
	if err != nil {
		return nil, err
	}
	return &Stream{transport: transport, inner: inner}, nil
}

// Read packs what the oldest transfers carried to the front of the buffer.
// Returns ErrTimeout when nothing arrived.
func (stream *Stream) Read(buffer []byte, timeoutMS uint32) (int, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed {
		return 0, ErrGone
	}
	stream.transport.mu.RLock()
	defer stream.transport.mu.RUnlock()
	if err := stream.transport.checkOpen(); err != nil {
		return 0, err
	}
	return stream.inner.Read(buffer, timeoutMS)
}

// Close stops the endpoint and frees the transfers. A second call is harmless.
func (stream *Stream) Close() error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed {
		return nil
	}
	stream.closed = true
	return stream.inner.Close()
}

// The shape of a bulk stream: sixteen transfers of 64 KB is a fifth of a second
// of the widest multiplex a USB tuner carries, and each transfer a whole number
// of high-speed packets.
const (
	bulkStreamSize  = 64 * 1024
	bulkStreamDepth = 16
)

// BulkReader reads a tuner's stream endpoint through a Stream where the backend
// has one, and a transfer at a time where it does not. The stream is opened by
// the first read and closed by Stop, so a bridge only has to say when its
// endpoint stops carrying anything.
type BulkReader struct {
	transport *Transport
	endpoint  byte

	mu     sync.Mutex
	stream *Stream
	// plain is set once the backend has said it has no ring.
	plain bool
	// carry holds what a transfer brought beyond a small caller's buffer.
	carry   []byte
	pending []byte
}

// BulkReader makes a reader for a stream endpoint. Nothing is opened yet.
func (transport *Transport) BulkReader(endpoint byte) *BulkReader {
	return &BulkReader{transport: transport, endpoint: endpoint}
}

// Start puts the reads in flight now rather than at the first Read. A bridge
// calls it just before it lets the stream go: the device's FIFO fills from that
// moment, and whatever arrives before anything is outstanding to take it is
// what the stream begins with a hole in.
func (reader *BulkReader) Start() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.open(streamTimeoutMS)
}

// streamTimeoutMS is what a stream opened by Start is given for its transfers.
const streamTimeoutMS = 1000

func (reader *BulkReader) open(timeoutMS uint32) error {
	if reader.stream != nil || reader.plain {
		return nil
	}
	stream, err := reader.transport.OpenBulkStream(reader.endpoint, bulkStreamSize, bulkStreamDepth, timeoutMS)
	switch {
	case errors.Is(err, ErrUnsupported):
		reader.plain = true
	case err != nil:
		return err
	default:
		reader.stream = stream
	}
	return nil
}

// Read takes the next bytes the endpoint carried. Returns ErrTimeout when the
// device had nothing ready.
func (reader *BulkReader) Read(buffer []byte, timeoutMS uint32) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.pending) != 0 {
		moved := copy(buffer, reader.pending)
		reader.pending = reader.pending[moved:]
		return moved, nil
	}
	if err := reader.open(timeoutMS); err != nil {
		return 0, err
	}
	if reader.plain {
		return reader.transport.Bulk(reader.endpoint, buffer, timeoutMS)
	}
	if len(buffer) >= bulkStreamSize {
		return reader.stream.Read(buffer, timeoutMS)
	}
	if reader.carry == nil {
		reader.carry = make([]byte, bulkStreamSize)
	}
	read, err := reader.stream.Read(reader.carry, timeoutMS)
	moved := copy(buffer, reader.carry[:read])
	reader.pending = reader.carry[moved:read]
	return moved, err
}

// Stop closes the stream, withdrawing whatever is still outstanding. The next
// Read opens it again.
func (reader *BulkReader) Stop() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.pending = nil
	if reader.stream == nil {
		return nil
	}
	err := reader.stream.Close()
	reader.stream = nil
	return err
}

// ClearHalt clears an endpoint's halt on the device and the host alike, which
// puts the two ends of its data toggle back in step.
func (transport *Transport) ClearHalt(endpoint byte) error {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if err := transport.checkOpen(); err != nil {
		return err
	}
	return transport.device.ClearHalt(endpoint)
}

// Close releases the interface and the device. A second call is harmless.
func (transport *Transport) Close() error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return nil
	}
	transport.closed = true
	return transport.device.Close()
}

// selected reports whether a selector picks this device: empty takes the first
// match, "bus:address" names one exactly, a bare number takes the nth match.
func selected(selector, location string, index int) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" || selector == location {
		return true
	}
	parsed, err := strconv.Atoi(selector)
	return err == nil && parsed == index
}

// EndpointReader reads a stream endpoint and is told when the stream starts
// and stops.
type EndpointReader interface {
	Start() error
	Read(buffer []byte, timeoutMS uint32) (int, error)
	Stop() error
}

// BulkDevice is anything that runs bulk transfers.
type BulkDevice interface {
	Bulk(endpoint byte, buffer []byte, timeoutMS uint32) (int, error)
}

// ReaderOf reads a stream endpoint through a BulkReader where the device is a
// Transport, and a transfer at a time where it is something standing in for
// one, as a test's fake is.
func ReaderOf(device BulkDevice, endpoint byte) EndpointReader {
	if transport, ok := device.(*Transport); ok {
		return transport.BulkReader(endpoint)
	}
	return plainReader{device: device, endpoint: endpoint}
}

type plainReader struct {
	device   BulkDevice
	endpoint byte
}

func (reader plainReader) Read(buffer []byte, timeoutMS uint32) (int, error) {
	return reader.device.Bulk(reader.endpoint, buffer, timeoutMS)
}

func (plainReader) Start() error { return nil }

func (plainReader) Stop() error { return nil }
