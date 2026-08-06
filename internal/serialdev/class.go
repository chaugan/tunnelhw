package serialdev

import (
	"fmt"

	"github.com/chaugan/tunnelhw/internal/device"
	"github.com/chaugan/tunnelhw/internal/proto"
)

// ClassName is how this class identifies itself on the wire and in the UI.
const ClassName = "serial"

// Class adapts serial ports to the class-agnostic device layer. Everything
// serial-specific stops here: baud rates, parity, and the DTR/RTS lines are
// described through device.ParamSpec and device.ActionSpec so the layers above
// can present them without understanding them.
type Class struct {
	// enumerate and open are injectable for tests; production uses the real
	// hardware paths.
	enumerate func() ([]PortInfo, error)
	open      Opener
}

// NewClass builds the serial class. Pass nil for the production behaviour.
func NewClass(enumerate func() ([]PortInfo, error), open Opener) *Class {
	if enumerate == nil {
		enumerate = Enumerate
	}
	if open == nil {
		open = Open
	}
	return &Class{enumerate: enumerate, open: open}
}

func (c *Class) Name() string { return ClassName }

func (c *Class) Enumerate() ([]device.Descriptor, error) {
	ports, err := c.enumerate()
	if err != nil {
		return nil, err
	}
	out := make([]device.Descriptor, 0, len(ports))
	for _, p := range ports {
		fp := FingerprintOf(p)
		meta := map[string]string{"path": p.Path}
		if p.Product != "" {
			meta["product"] = p.Product
		}
		if p.VID != "" {
			meta["vid"], meta["pid"] = p.VID, p.PID
		}
		if p.SerialNumber != "" {
			meta["serial_number"] = p.SerialNumber
		}
		out = append(out, device.Descriptor{
			Class:   ClassName,
			Address: p.Path,
			Fingerprint: device.Fingerprint{
				Key:        fp.Key,
				Confidence: fp.Confidence,
				Transport:  fp.Transport,
			},
			Meta: meta,
		})
	}
	return out, nil
}

// OpenParams describes what a serial open accepts.
func (c *Class) OpenParams() []device.ParamSpec {
	return []device.ParamSpec{
		{Name: "baud", Type: "int", Default: 115200,
			Description: "bit rate, e.g. 9600, 115200"},
		{Name: "data_bits", Type: "int", Default: 8,
			Description: "bits per character, usually 8"},
		{Name: "parity", Type: "string", Default: "none", Enum: []string{"none", "odd", "even"},
			Description: "parity checking"},
		{Name: "stop_bits", Type: "string", Default: "1", Enum: []string{"1", "1.5", "2"},
			Description: "stop bits"},
	}
}

// Actions describes the control actions a serial connection supports. All of
// them are privileged: changing the line rate mid-session or moving DTR/RTS
// can reset a board or drop it into its bootloader.
func (c *Class) Actions() []device.ActionSpec {
	return []device.ActionSpec{
		{
			Name:       "set_line",
			Privileged: true,
			Description: "Change line parameters mid-session. Toggling DTR or RTS can " +
				"physically reset a board or put it into its bootloader.",
			Params: []device.ParamSpec{
				{Name: "baud", Type: "int", Description: "new bit rate; omit to leave unchanged"},
				{Name: "dtr", Type: "bool", Description: "set the DTR line"},
				{Name: "rts", Type: "bool", Description: "set the RTS line"},
			},
		},
	}
}

func (c *Class) Open(d device.Descriptor, params map[string]any, assertPrivileged bool) (device.Conn, error) {
	if d.Class != ClassName {
		return nil, fmt.Errorf("serialdev: descriptor is class %q", d.Class)
	}
	port, err := c.open(d.Address, proto.OpenParams{
		Baud:     device.Int(params, "baud", 115200),
		DataBits: device.Int(params, "data_bits", 8),
		Parity:   device.Str(params, "parity", "none"),
		StopBits: device.Str(params, "stop_bits", "1"),
		// Raising DTR/RTS at open resets boards wired for auto-reset, so it
		// happens only where the operator granted privileged actions and asked
		// for it.
		AssertLinesOnOpen: assertPrivileged,
	})
	if err != nil {
		return nil, err
	}
	return &conn{Port: port}, nil
}

// conn adapts a serial Port to the class-agnostic Conn.
type conn struct{ Port }

func (c *conn) Control(action string, args map[string]any) error {
	if action != "set_line" {
		return fmt.Errorf("serial: unknown action %q", action)
	}
	var baud *int
	var dtr, rts *bool
	if v, ok := args["baud"]; ok {
		n, ok := toInt(v)
		if !ok {
			return fmt.Errorf("serial: baud must be an integer, got %T", v)
		}
		baud = &n
	}
	if v, ok := args["dtr"]; ok {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("serial: dtr must be a boolean, got %T", v)
		}
		dtr = &b
	}
	if v, ok := args["rts"]; ok {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("serial: rts must be a boolean, got %T", v)
		}
		rts = &b
	}
	if baud == nil && dtr == nil && rts == nil {
		return fmt.Errorf("serial: set_line needs at least one of baud, dtr, rts")
	}
	return c.SetParams(baud, dtr, rts)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64: // JSON numbers
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}
