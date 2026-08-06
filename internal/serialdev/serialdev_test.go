package serialdev

import (
	"testing"

	"github.com/chaugan/tunnelhw/internal/proto"
)

func TestFingerprintTiers(t *testing.T) {
	cases := []struct {
		name      string
		goos      string
		p         PortInfo
		wantConf  string
		wantTrans string
	}{
		{"usb with serial", "linux", PortInfo{Path: "/dev/ttyUSB0", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A6008isP"}, proto.ConfidenceStrong, proto.TransportUSB},
		{"usb no serial", "linux", PortInfo{Path: "/dev/ttyUSB1", IsUSB: true, VID: "1a86", PID: "7523"}, proto.ConfidenceWeak, proto.TransportUSB},
		{"linux native uart", "linux", PortInfo{Path: "/dev/ttyS0"}, proto.ConfidenceMedium, proto.TransportNative},
		{"linux pi uart", "linux", PortInfo{Path: "/dev/ttyAMA0"}, proto.ConfidenceMedium, proto.TransportNative},
		{"linux bluetooth", "linux", PortInfo{Path: "/dev/rfcomm0"}, proto.ConfidenceWeak, proto.TransportBluetooth},
		{"windows native com", "windows", PortInfo{Path: "COM1"}, proto.ConfidenceMedium, proto.TransportNative},
		{"windows usb com no sn", "windows", PortInfo{Path: "COM7", IsUSB: true, VID: "1a86", PID: "7523"}, proto.ConfidenceWeak, proto.TransportUSB},
		{"mac bluetooth", "darwin", PortInfo{Path: "/dev/cu.Bluetooth-Incoming-Port"}, proto.ConfidenceWeak, proto.TransportBluetooth},
		{"unknown", "linux", PortInfo{Path: "/dev/ttyWeird9"}, proto.ConfidenceWeak, proto.TransportUnknown},
	}
	for _, c := range cases {
		fp := fingerprintFor(c.p, c.goos)
		if fp.Confidence != c.wantConf || fp.Transport != c.wantTrans {
			t.Errorf("%s: got (%s,%s), want (%s,%s)", c.name, fp.Confidence, fp.Transport, c.wantConf, c.wantTrans)
		}
		if fp.Key == "" {
			t.Errorf("%s: empty key", c.name)
		}
	}
}

func TestFingerprintStableAcrossPathForStrong(t *testing.T) {
	a := fingerprintFor(PortInfo{Path: "/dev/ttyUSB0", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "SN1"}, "linux")
	b := fingerprintFor(PortInfo{Path: "/dev/ttyUSB3", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "SN1"}, "linux")
	if a.Key != b.Key {
		t.Errorf("strong fingerprint must survive path renumbering: %q != %q", a.Key, b.Key)
	}
}

func TestModeFor(t *testing.T) {
	if _, err := modeFor(proto.OpenParams{Baud: 0}); err == nil {
		t.Error("baud 0 must be rejected")
	}
	if _, err := modeFor(proto.OpenParams{Baud: 9600, Parity: "banana"}); err == nil {
		t.Error("bad parity must be rejected")
	}
	m, err := modeFor(proto.OpenParams{Baud: 115200, DataBits: 7, Parity: "even", StopBits: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if m.BaudRate != 115200 || m.DataBits != 7 {
		t.Errorf("mode = %+v", m)
	}
}

// The library raises DTR and RTS when InitialStatusBits is nil, which resets
// any board wired for auto-reset the moment the port opens. Opening a device
// must not touch the hardware unless the operator asked for it.
func TestOpenDoesNotAssertControlLinesByDefault(t *testing.T) {
	m, err := modeFor(proto.OpenParams{Baud: 115200})
	if err != nil {
		t.Fatal(err)
	}
	if m.InitialStatusBits == nil {
		t.Fatal("InitialStatusBits is nil, so the library will raise DTR and RTS and reset the board")
	}
	if m.InitialStatusBits.DTR || m.InitialStatusBits.RTS {
		t.Fatalf("opening must leave the control lines alone: DTR=%v RTS=%v",
			m.InitialStatusBits.DTR, m.InitialStatusBits.RTS)
	}

	// Devices that need the lines raised (some USB-CDC firmware only
	// transmits once DTR is set) opt in per device.
	m, err = modeFor(proto.OpenParams{Baud: 115200, AssertLinesOnOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if !m.InitialStatusBits.DTR || !m.InitialStatusBits.RTS {
		t.Fatalf("opt-in must raise both lines: DTR=%v RTS=%v",
			m.InitialStatusBits.DTR, m.InitialStatusBits.RTS)
	}
}
