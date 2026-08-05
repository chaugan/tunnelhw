package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/serialdev"
)

// slowPort simulates hardware that takes a moment to open, which is the window
// the revocation race lived in.
type slowPort struct {
	mu     sync.Mutex
	closed bool
}

func (p *slowPort) Read([]byte) (int, error)           { select {} }
func (p *slowPort) Write(b []byte) (int, error)        { return len(b), nil }
func (p *slowPort) Drain() error                       { return nil }
func (p *slowPort) SetParams(*int, *bool, *bool) error { return nil }
func (p *slowPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}
func (p *slowPort) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Opening the hardware happens outside the lock, so an operator can hide a
// device while a session is still being established. Registering that session
// anyway would resurrect access that was just revoked.
func TestHideDuringOpenIsNotResurrected(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ports := []serialdev.PortInfo{{Path: "/dev/ttySLOW", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "SLOW1"}}

	opening := make(chan struct{})
	release := make(chan struct{})
	port := &slowPort{}
	core := New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { return ports, nil },
		func(string, proto.OpenParams) (serialdev.Port, error) {
			close(opening)
			<-release // hold the "hardware" open until the test says go
			return port, nil
		})
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}
	dev := core.UIDevices()[0]
	if err := core.SetExposed(dev.UUID, true); err != nil {
		t.Fatal(err)
	}

	type result struct {
		s    *Session
		resp *proto.OpenResponse
	}
	done := make(chan result, 1)
	go func() {
		s, resp := core.OpenSession(dev.ID, proto.OpenParams{Baud: 115200})
		done <- result{s, resp}
	}()

	<-opening // the port is mid-open
	if err := core.SetExposed(dev.UUID, false); err != nil {
		t.Fatal(err)
	}
	close(release)

	select {
	case r := <-done:
		if r.s != nil || r.resp.OK {
			t.Fatal("a session was created for a device hidden while it was opening")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenSession did not return")
	}
	if !port.isClosed() {
		t.Fatal("the serial port was left open after the aborted session")
	}
	if n := len(core.Sessions()); n != 0 {
		t.Fatalf("sessions = %d, want 0", n)
	}
	// The claim must be released so the device is usable again once re-exposed.
	if err := core.SetExposed(dev.UUID, true); err != nil {
		t.Fatal(err)
	}
	if core.ReleaseDevice(dev.UUID, "check") {
		t.Fatal("a stale claim was left behind")
	}
}
