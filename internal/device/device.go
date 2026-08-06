// Package device is the class-agnostic device layer.
//
// Everything above this package moves opaque bytes: the tunnel, the session
// model, the relay and the MCP adapter have no notion of what a device is.
// Everything a particular kind of hardware needs to know, such as what baud
// means or which control actions exist, lives behind Class and never leaks
// upward. Adding a device class means implementing Class and registering it,
// with no change to the protocol, the relay or the tools.
package device

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// Confidence describes how reliably a fingerprint follows the same physical
// device across reconnects.
const (
	ConfidenceStrong = "strong"
	ConfidenceMedium = "medium"
	ConfidenceWeak   = "weak"
)

// Fingerprint is a device's stable identity, with an honest label for how
// much that identity can be trusted.
type Fingerprint struct {
	// Key is unique per physical device and stable across reconnects. It is
	// namespaced by scheme so two classes cannot collide.
	Key        string
	Confidence string
	// Transport is a human-facing hint ("usb", "native", "tcp"), not dispatch.
	Transport string
}

// Descriptor is one discovered device, described without reference to any
// particular class.
type Descriptor struct {
	Class       string            // the Class that produced and can open it
	Address     string            // class-private handle: a port path, a host:port
	Fingerprint Fingerprint       //
	Meta        map[string]string // display metadata, shown but never interpreted
}

// Conn is an open device. Read and Write carry the bytes; everything
// class-specific goes through Control.
type Conn interface {
	io.ReadWriteCloser
	// Drain blocks until bytes already written have reached the hardware.
	Drain() error
	// Control performs a class-specific action. Unknown actions must be
	// rejected, never ignored.
	Control(action string, args map[string]any) error
}

// ParamSpec documents one open parameter so the tool layer can describe it to
// an LLM without knowing what it means.
type ParamSpec struct {
	Name        string
	Type        string // "int" | "string" | "bool"
	Description string
	Default     any
	Enum        []string // when the value is constrained
}

// ActionSpec documents one control action, and whether it is privileged.
type ActionSpec struct {
	Name        string
	Description string
	Params      []ParamSpec
	// Privileged actions can physically disrupt the device (reset a board,
	// power-cycle a port) and require the operator's per-device grant.
	Privileged bool
}

// Class is one kind of device. Implementations are the only place that knows
// what its hardware means.
type Class interface {
	// Name identifies the class on the wire and in the UI, e.g. "serial".
	Name() string
	// Enumerate lists devices currently present.
	Enumerate() ([]Descriptor, error)
	// Open connects to a device. params has already been validated against
	// OpenParams; assertPrivileged reports whether the operator has granted
	// this device its privileged actions, which some classes need at open.
	Open(d Descriptor, params map[string]any, assertPrivileged bool) (Conn, error)
	// OpenParams documents what Open accepts.
	OpenParams() []ParamSpec
	// Actions documents what Conn.Control accepts.
	Actions() []ActionSpec
}

var (
	mu      sync.RWMutex
	classes = map[string]Class{}
)

// Register adds a class. It panics on a duplicate name, since that is a
// programming error rather than a runtime condition.
func Register(c Class) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := classes[c.Name()]; dup {
		panic("device: class registered twice: " + c.Name())
	}
	classes[c.Name()] = c
}

// Get returns a registered class.
func Get(name string) (Class, error) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := classes[name]
	if !ok {
		return nil, fmt.Errorf("device: no such class %q", name)
	}
	return c, nil
}

// Classes lists registered classes, ordered for stable output.
func Classes() []Class {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Class, 0, len(classes))
	for _, c := range classes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// EnumerateAll gathers devices from every registered class. A class that fails
// to enumerate does not hide the others: its error is returned alongside
// whatever the rest found.
func EnumerateAll() ([]Descriptor, error) {
	var out []Descriptor
	var firstErr error
	for _, c := range Classes() {
		found, err := c.Enumerate()
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", c.Name(), err)
			continue
		}
		out = append(out, found...)
	}
	return out, firstErr
}

// ValidateOpenParams checks raw params against a class's declared spec,
// filling in defaults. Unknown keys are rejected so a typo fails loudly
// instead of being silently ignored.
func ValidateOpenParams(c Class, raw map[string]any) (map[string]any, error) {
	specs := c.OpenParams()
	byName := make(map[string]ParamSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	for k := range raw {
		if _, ok := byName[k]; !ok {
			return nil, fmt.Errorf("%s: unknown parameter %q", c.Name(), k)
		}
	}
	out := make(map[string]any, len(specs))
	for _, s := range specs {
		v, given := raw[s.Name]
		if !given {
			if s.Default != nil {
				out[s.Name] = s.Default
			}
			continue
		}
		cv, err := coerce(s, v)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", c.Name(), s.Name, err)
		}
		out[s.Name] = cv
	}
	return out, nil
}

// coerce normalises a JSON-decoded value to the declared type. JSON numbers
// arrive as float64, so integers need converting back.
func coerce(s ParamSpec, v any) (any, error) {
	switch s.Type {
	case "int":
		switch n := v.(type) {
		case int:
			return n, nil
		case float64:
			if n != float64(int(n)) {
				return nil, fmt.Errorf("want a whole number, got %v", n)
			}
			return int(n), nil
		}
		return nil, fmt.Errorf("want an integer, got %T", v)
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("want a boolean, got %T", v)
		}
		return b, nil
	case "string":
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("want a string, got %T", v)
		}
		if len(s.Enum) > 0 {
			for _, allowed := range s.Enum {
				if str == allowed {
					return str, nil
				}
			}
			return nil, fmt.Errorf("want one of %v, got %q", s.Enum, str)
		}
		return str, nil
	}
	return nil, fmt.Errorf("unsupported parameter type %q", s.Type)
}

// Int reads an integer parameter, falling back to def.
func Int(params map[string]any, name string, def int) int {
	if v, ok := params[name].(int); ok {
		return v
	}
	return def
}

// Str reads a string parameter, falling back to def.
func Str(params map[string]any, name, def string) string {
	if v, ok := params[name].(string); ok && v != "" {
		return v
	}
	return def
}

// Bool reads a boolean parameter, falling back to def.
func Bool(params map[string]any, name string, def bool) bool {
	if v, ok := params[name].(bool); ok {
		return v
	}
	return def
}
