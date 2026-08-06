// Package serialdev enumerates, fingerprints, and opens serial ports of any
// transport: USB adapters, native COM ports / UARTs, PCI serial cards, and
// Bluetooth SPP virtual ports. USB is one transport among several, never an
// assumption.
package serialdev

import (
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strings"

	"github.com/chaugan/tunnelhw/internal/proto"
	"go.bug.st/serial"
)

// PortInfo is one enumerated serial port.
type PortInfo struct {
	Path         string
	IsUSB        bool
	VID          string
	PID          string
	SerialNumber string
	Product      string
}

// Enumerate lists every serial port the OS knows about. The implementation is
// build-tagged: everywhere except cgo-less darwin it uses the detailed
// enumerator (USB metadata included); a darwin binary cross-compiled without
// cgo falls back to bare port paths with degraded metadata (see
// enumerate_fallback.go).
func Enumerate() ([]PortInfo, error) { return enumerate() }

// Fingerprint is a device's stable identity with an honesty label.
type Fingerprint struct {
	Key        string // stable map key, namespaced by scheme
	Confidence string // proto.ConfidenceStrong|Medium|Weak
	Transport  string // proto.TransportUSB|Native|Bluetooth|Unknown
}

var (
	linuxNative = regexp.MustCompile(`^/dev/tty(S|AMA|SAC)\d+$`)
	linuxBlue   = regexp.MustCompile(`^/dev/rfcomm\d+$`)
	darwinBlue  = regexp.MustCompile(`(?i)^/dev/(cu|tty)\..*bluetooth`)
	windowsCOM  = regexp.MustCompile(`^COM(\d+)$`)
)

// FingerprintOf derives the tiered fingerprint for a port (see
// ARCHITECTURE.md §5.2). Pure function; unit-testable per platform via goos.
func FingerprintOf(p PortInfo) Fingerprint {
	return fingerprintFor(p, runtime.GOOS)
}

func fingerprintFor(p PortInfo, goos string) Fingerprint {
	// Strong: a USB device that reports a serial number.
	if p.IsUSB && p.SerialNumber != "" {
		return Fingerprint{
			Key:        "usb-sn:" + p.VID + ":" + p.PID + ":" + p.SerialNumber,
			Confidence: proto.ConfidenceStrong,
			Transport:  proto.TransportUSB,
		}
	}
	// USB without a serial number: identity is VID:PID + current path, which
	// is weak because it renumbers when devices come and go.
	if p.IsUSB {
		return Fingerprint{
			Key:        "usb:" + p.VID + ":" + p.PID + ":" + p.Path,
			Confidence: proto.ConfidenceWeak,
			Transport:  proto.TransportUSB,
		}
	}
	// Non-USB transports.
	switch goos {
	case "linux":
		if linuxBlue.MatchString(p.Path) {
			return Fingerprint{Key: "port:" + p.Path, Confidence: proto.ConfidenceWeak, Transport: proto.TransportBluetooth}
		}
		if linuxNative.MatchString(p.Path) {
			// Motherboard UART / PCI card: the platform name is the hardware
			// identity and does not move.
			return Fingerprint{Key: "port:" + p.Path, Confidence: proto.ConfidenceMedium, Transport: proto.TransportNative}
		}
	case "darwin":
		if darwinBlue.MatchString(p.Path) {
			return Fingerprint{Key: "port:" + p.Path, Confidence: proto.ConfidenceWeak, Transport: proto.TransportBluetooth}
		}
	case "windows":
		if m := windowsCOM.FindStringSubmatch(p.Path); m != nil {
			// A non-USB COM port is native hardware (UART / PCI card); its
			// COM number is assigned by firmware and stable.
			return Fingerprint{Key: "port:" + p.Path, Confidence: proto.ConfidenceMedium, Transport: proto.TransportNative}
		}
	}
	return Fingerprint{Key: "port:" + p.Path, Confidence: proto.ConfidenceWeak, Transport: proto.TransportUnknown}
}

// Port is an open serial session. It narrows go.bug.st's interface to what
// the bridge needs, so tests can substitute an in-memory fake.
type Port interface {
	io.ReadWriteCloser
	SetParams(baud *int, dtr, rts *bool) error
	Drain() error
}

// Opener opens a port by path. The production implementation drives real
// hardware; tests inject fakes.
type Opener func(path string, p proto.OpenParams) (Port, error)

// Open is the production Opener.
func Open(path string, p proto.OpenParams) (Port, error) {
	mode, err := modeFor(p)
	if err != nil {
		return nil, err
	}
	sp, err := serial.Open(path, mode)
	if err != nil {
		return nil, fmt.Errorf("serialdev: open %s: %w", path, err)
	}
	return &realPort{p: sp, mode: *mode}, nil
}

func modeFor(p proto.OpenParams) (*serial.Mode, error) {
	// The library raises DTR and RTS when InitialStatusBits is nil. On any
	// board wired for auto-reset (DTR/RTS to EN/IO0, which is every ESP32 and
	// Arduino dev board) that resets the chip the instant the port opens, so
	// merely reading from a device would reboot it. Leave the lines alone
	// unless per-device policy says otherwise.
	m := &serial.Mode{
		BaudRate: p.Baud, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit,
		InitialStatusBits: &serial.ModemOutputBits{
			DTR: p.AssertLinesOnOpen,
			RTS: p.AssertLinesOnOpen,
		},
	}
	if p.Baud <= 0 {
		return nil, fmt.Errorf("serialdev: invalid baud %d", p.Baud)
	}
	if p.DataBits != 0 {
		m.DataBits = p.DataBits
	}
	switch strings.ToLower(p.Parity) {
	case "", "none":
	case "odd":
		m.Parity = serial.OddParity
	case "even":
		m.Parity = serial.EvenParity
	default:
		return nil, fmt.Errorf("serialdev: invalid parity %q", p.Parity)
	}
	switch p.StopBits {
	case "", "1":
	case "1.5":
		m.StopBits = serial.OnePointFiveStopBits
	case "2":
		m.StopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("serialdev: invalid stop bits %q", p.StopBits)
	}
	return m, nil
}

type realPort struct {
	p    serial.Port
	mode serial.Mode
}

func (r *realPort) Read(b []byte) (int, error)  { return r.p.Read(b) }
func (r *realPort) Write(b []byte) (int, error) { return r.p.Write(b) }
func (r *realPort) Close() error                { return r.p.Close() }
func (r *realPort) Drain() error                { return r.p.Drain() }

func (r *realPort) SetParams(baud *int, dtr, rts *bool) error {
	if baud != nil {
		next := r.mode
		next.BaudRate = *baud
		if err := r.p.SetMode(&next); err != nil {
			return err
		}
		r.mode = next
	}
	if dtr != nil {
		if err := r.p.SetDTR(*dtr); err != nil {
			return err
		}
	}
	if rts != nil {
		if err := r.p.SetRTS(*rts); err != nil {
			return err
		}
	}
	return nil
}
