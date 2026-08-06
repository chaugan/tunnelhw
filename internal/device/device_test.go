package device

import (
	"errors"
	"testing"
)

// fakeClass stands in for a device class that is not serial, which is the
// point: nothing above this package should need to know the difference.
type fakeClass struct{ name string }

func (f fakeClass) Name() string { return f.name }
func (f fakeClass) Enumerate() ([]Descriptor, error) {
	return []Descriptor{{Class: f.name, Address: "somewhere",
		Fingerprint: Fingerprint{Key: f.name + ":1", Confidence: ConfidenceStrong, Transport: "test"}}}, nil
}
func (f fakeClass) Open(Descriptor, map[string]any, bool) (Conn, error) {
	return nil, errors.New("not needed for this test")
}
func (f fakeClass) OpenParams() []ParamSpec {
	return []ParamSpec{
		{Name: "port", Type: "int", Default: 5025, Description: "TCP port"},
		{Name: "mode", Type: "string", Default: "raw", Enum: []string{"raw", "telnet"}},
		{Name: "tls", Type: "bool", Default: false},
	}
}
func (f fakeClass) Actions() []ActionSpec { return nil }

func TestValidateOpenParams(t *testing.T) {
	c := fakeClass{"tcp"}

	// Defaults are filled in when the caller says nothing.
	got, err := ValidateOpenParams(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["port"] != 5025 || got["mode"] != "raw" || got["tls"] != false {
		t.Fatalf("defaults not applied: %+v", got)
	}

	// JSON numbers arrive as float64 and must come back as int.
	got, err = ValidateOpenParams(c, map[string]any{"port": float64(9600)})
	if err != nil {
		t.Fatal(err)
	}
	if got["port"] != 9600 {
		t.Fatalf("port = %#v, want int 9600", got["port"])
	}

	// A typo must fail loudly rather than being silently dropped.
	if _, err := ValidateOpenParams(c, map[string]any{"prot": 1}); err == nil {
		t.Fatal("unknown parameter must be rejected")
	}
	// Enums are enforced.
	if _, err := ValidateOpenParams(c, map[string]any{"mode": "carrier-pigeon"}); err == nil {
		t.Fatal("value outside the enum must be rejected")
	}
	// So are wrong types and non-whole numbers.
	if _, err := ValidateOpenParams(c, map[string]any{"port": "9600"}); err == nil {
		t.Fatal("string for an int parameter must be rejected")
	}
	if _, err := ValidateOpenParams(c, map[string]any{"port": 96.5}); err == nil {
		t.Fatal("fractional value for an int parameter must be rejected")
	}
}

func TestRegistry(t *testing.T) {
	Register(fakeClass{"tcp-test"})
	c, err := Get("tcp-test")
	if err != nil || c.Name() != "tcp-test" {
		t.Fatalf("Get: %v %v", c, err)
	}
	if _, err := Get("nope"); err == nil {
		t.Fatal("unknown class must error")
	}
	found := false
	for _, c := range Classes() {
		if c.Name() == "tcp-test" {
			found = true
		}
	}
	if !found {
		t.Fatal("registered class missing from Classes()")
	}
}
