// Package e2e wires the real agent (core + tunnel) to the real relay
// (hub + broker) over a live WebSocket, with an in-memory serial device.
// This is the hardware-free proof of the whole pipeline.
package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaugan/tunnelhw/internal/agent"
	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/relay"
	"github.com/chaugan/tunnelhw/internal/relayapi"
	"github.com/chaugan/tunnelhw/internal/serialdev"
)

// fakePort echoes writes back to reads and records SetParams calls.
type fakePort struct {
	mu     sync.Mutex
	pr     *io.PipeReader
	pw     *io.PipeWriter
	params []string
	closed bool
}

func newFakePort() *fakePort {
	pr, pw := io.Pipe()
	return &fakePort{pr: pr, pw: pw}
}

func (f *fakePort) Read(b []byte) (int, error)  { return f.pr.Read(b) }
func (f *fakePort) Write(b []byte) (int, error) { return f.pw.Write(b) } // echo
func (f *fakePort) Drain() error                { return nil }
func (f *fakePort) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.pw.Close()
		f.pr.Close()
	}
	return nil
}
func (f *fakePort) SetParams(baud *int, dtr, rts *bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.params = append(f.params, "set")
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEnd(t *testing.T) {
	// Relay side.
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Pair an agent the proper way: mint + exchange.
	tok, _, err := store.MintPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	agentID, cred, err := store.ExchangePairing(tok, "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	// Agent side: one fake USB serial device.
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ports := []serialdev.PortInfo{{Path: "/dev/ttyFAKE0", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "TESTSN1", Product: "Fake Board"}}
	var openedPort *fakePort
	var portMu sync.Mutex
	core := agent.New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { return ports, nil },
		func(path string, p proto.OpenParams) (serialdev.Port, error) {
			portMu.Lock()
			defer portMu.Unlock()
			openedPort = newFakePort()
			return openedPort, nil
		})
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}

	devs := core.UIDevices()
	if len(devs) != 1 {
		t.Fatalf("UIDevices = %d, want 1", len(devs))
	}
	wordID := devs[0].ID
	if !strings.Contains(wordID, "-") {
		t.Fatalf("word id %q", wordID)
	}
	if devs[0].Meta.FingerprintConfidence != proto.ConfidenceStrong {
		t.Fatalf("confidence = %s", devs[0].Meta.FingerprintConfidence)
	}

	// Nothing exposed yet → relay must see zero devices even once connected.
	tun := &agent.Tunnel{
		Core:        core,
		URL:         "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		AgentID:     agentID,
		Credential:  cred,
		InsecureDev: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Run(ctx)

	waitFor(t, "tunnel connected", func() bool { s, _ := tun.Status(); return s == "connected" })
	time.Sleep(100 * time.Millisecond)
	if n := len(hub.Devices(nil)); n != 0 {
		t.Fatalf("unexposed device leaked to relay: %d devices", n)
	}

	// Expose → announce → visible.
	if err := core.SetExposed(devs[0].UUID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "device announced", func() bool { return len(hub.Devices(nil)) == 1 })

	// Open, write, read echo.
	sess, err := broker.Open(wordID, proto.OpenParams{Baud: 115200}, nil, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Write([]byte("hello hardware\n")); err != nil {
		t.Fatal(err)
	}
	res := sess.Read(5*time.Second, 1024, []byte("\n"))
	if got := string(res.Data); got != "hello hardware\n" {
		t.Fatalf("read %q (timed_out=%v eof=%v)", got, res.TimedOut, res.EOF)
	}

	// Exclusive open: second open must fail busy.
	if _, err := broker.Open(wordID, proto.OpenParams{Baud: 9600}, nil, "test-owner"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("second open: err = %v, want busy", err)
	}

	// Control lines denied without grant…
	if err := broker.SetParams(sess.ID, nil, boolPtr(true), nil); err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("set_params without grant: err = %v", err)
	}
	// …allowed with it.
	if err := core.SetControlLines(devs[0].UUID, true); err != nil {
		t.Fatal(err)
	}
	if err := broker.SetParams(sess.ID, nil, boolPtr(true), nil); err != nil {
		t.Fatalf("set_params with grant: %v", err)
	}

	// Close frees the device for a fresh open.
	if err := broker.Close(sess.ID); err != nil {
		t.Fatal(err)
	}
	// The agent releases the claim when it processes the close notification,
	// so the device frees up shortly after Close returns, not synchronously.
	var s3 *relayapi.Session
	waitFor(t, "device free after close", func() bool {
		var err error
		s3, err = broker.Open(wordID, proto.OpenParams{Baud: 115200}, nil, "test-owner")
		return err == nil
	})

	// Read timeout semantics: no data → timed_out, empty.
	r := s3.Read(100*time.Millisecond, 64, nil)
	if !r.TimedOut || len(r.Data) != 0 {
		t.Fatalf("timeout read = %+v", r)
	}
	broker.Close(s3.ID)
}

func boolPtr(b bool) *bool { return &b }

// Hiding a device must also end any session already holding it, and the
// per-device release must free the port without disturbing the tunnel.
// Otherwise "revoke access" in the UI is a lie: the consumer keeps streaming.
func TestHideAndReleaseFreeTheDevice(t *testing.T) {
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tok, _, _ := store.MintPairingToken()
	agentID, cred, err := store.ExchangePairing(tok, "t")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg, _ := config.Load(dir)
	ports := []serialdev.PortInfo{{Path: "/dev/ttyFAKE9", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "SN9"}}
	var mu sync.Mutex
	var last *fakePort
	core := agent.New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { return ports, nil },
		func(path string, p proto.OpenParams) (serialdev.Port, error) {
			mu.Lock()
			defer mu.Unlock()
			last = newFakePort()
			return last, nil
		})
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}
	dev := core.UIDevices()[0]
	if err := core.SetExposed(dev.UUID, true); err != nil {
		t.Fatal(err)
	}

	tun := &agent.Tunnel{Core: core, URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		AgentID: agentID, Credential: cred, InsecureDev: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Run(ctx)
	waitFor(t, "connected", func() bool { s, _ := tun.Status(); return s == "connected" })
	waitFor(t, "announced", func() bool { return len(hub.Devices(nil)) == 1 })

	// --- hiding a device kills the live session ---
	s1, err := broker.Open(dev.ID, proto.OpenParams{Baud: 115200}, nil, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := core.SetExposed(dev.UUID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session ended by hiding", func() bool { return len(core.Sessions()) == 0 })
	mu.Lock()
	closed := last.closed
	mu.Unlock()
	if !closed {
		t.Fatal("hiding a device must close the serial port, not just stop announcing it")
	}
	// The consumer's reads must stop yielding device data.
	if r := s1.Read(500*time.Millisecond, 64, nil); len(r.Data) != 0 {
		t.Fatalf("data still flowing after hide: %q", r.Data)
	}
	broker.Close(s1.ID)

	// --- release frees a device without hiding it or dropping the tunnel ---
	if err := core.SetExposed(dev.UUID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-announced", func() bool { return len(hub.Devices(nil)) == 1 })
	var s2 *relayapi.Session
	waitFor(t, "reopen", func() bool {
		var err error
		s2, err = broker.Open(dev.ID, proto.OpenParams{Baud: 115200}, nil, "owner")
		return err == nil
	})
	if !core.ReleaseDevice(dev.UUID, "test") {
		t.Fatal("ReleaseDevice reported no session to release")
	}
	waitFor(t, "released", func() bool { return len(core.Sessions()) == 0 })
	if state, _ := tun.Status(); state != "connected" {
		t.Fatalf("release must not disturb the tunnel; state = %s", state)
	}
	// Still exposed, and immediately reusable.
	waitFor(t, "device reusable after release", func() bool {
		s3, err := broker.Open(dev.ID, proto.OpenParams{Baud: 115200}, nil, "owner")
		if err != nil {
			return false
		}
		broker.Close(s3.ID)
		return true
	})
	// Close notifies the agent asynchronously, so the claim can still be held
	// for a moment. Wait for it to drop before asserting there is nothing left
	// to release; otherwise this assertion is a coin flip.
	waitFor(t, "claim dropped after close", func() bool { return len(core.Sessions()) == 0 })
	if core.ReleaseDevice(dev.UUID, "test") {
		t.Fatal("releasing an idle device must report nothing to release")
	}
	_ = s2
}

// The kill switch must latch. Before this, Disconnect only severed the current
// session and the retry loop reconnected a second later, re-announcing every
// exposed device, a safety control that quietly undid itself.
func TestKillSwitchStaysOff(t *testing.T) {
	store, _ := auth.Open(t.TempDir())
	hub := relay.NewHub(store)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tok, _, _ := store.MintPairingToken()
	agentID, cred, err := store.ExchangePairing(tok, "t")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg, _ := config.Load(dir)
	ports := []serialdev.PortInfo{{Path: "/dev/ttyFAKE7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "SN7"}}
	core := agent.New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { return ports, nil },
		func(string, proto.OpenParams) (serialdev.Port, error) { return newFakePort(), nil })
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}
	if err := core.SetExposed(core.UIDevices()[0].UUID, true); err != nil {
		t.Fatal(err)
	}

	tun := &agent.Tunnel{Core: core, URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		AgentID: agentID, Credential: cred, InsecureDev: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Run(ctx)
	waitFor(t, "connected", func() bool { s, _ := tun.Status(); return s == "connected" })
	waitFor(t, "announced", func() bool { return len(hub.Devices(nil)) == 1 })

	tun.Disconnect()
	waitFor(t, "tunnel reports stopped", func() bool { s, _ := tun.Status(); return s == "stopped" })
	waitFor(t, "devices withdrawn", func() bool { return len(hub.Devices(nil)) == 0 })

	// The old bug: backoff starts at 1s, so anything under a few seconds would
	// have silently reconnected by now.
	time.Sleep(3 * time.Second)
	if s, _ := tun.Status(); s != "stopped" {
		t.Fatalf("kill switch did not latch: state = %q", s)
	}
	if n := len(hub.Devices(nil)); n != 0 {
		t.Fatalf("devices re-announced while stopped: %d", n)
	}

	tun.Resume()
	waitFor(t, "reconnected after resume", func() bool { s, _ := tun.Status(); return s == "connected" })
	waitFor(t, "re-announced after resume", func() bool { return len(hub.Devices(nil)) == 1 })
}

// A device that vanishes (unplug, or the USB re-enumeration that follows a
// firmware flash) must take its session with it. Otherwise the consumer is
// stranded on a dead handle where reads return nothing for ever, which looks
// exactly like a device that has nothing to say. That ambiguity destroys the
// evidence an operator is usually trying to gather.
func TestVanishedDeviceEndsItsSession(t *testing.T) {
	store, _ := auth.Open(t.TempDir())
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tok, _, _ := store.MintPairingToken()
	agentID, cred, err := store.ExchangePairing(tok, "t")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg, _ := config.Load(dir)
	present := []serialdev.PortInfo{{Path: "/dev/ttyGONE", IsUSB: true, VID: "303a", PID: "1001", SerialNumber: "FLASH1"}}
	var mu sync.Mutex
	core := agent.New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { mu.Lock(); defer mu.Unlock(); return present, nil },
		func(string, proto.OpenParams) (serialdev.Port, error) { return newFakePort(), nil })
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}
	dev := core.UIDevices()[0]
	if err := core.SetExposed(dev.UUID, true); err != nil {
		t.Fatal(err)
	}

	tun := &agent.Tunnel{Core: core, URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		AgentID: agentID, Credential: cred, InsecureDev: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Run(ctx)
	waitFor(t, "connected", func() bool { s, _ := tun.Status(); return s == "connected" })
	waitFor(t, "announced", func() bool { return len(hub.Devices(nil)) == 1 })

	sess, err := broker.Open(dev.ID, proto.OpenParams{Baud: 115200}, nil, "owner")
	if err != nil {
		t.Fatal(err)
	}

	// The board re-enumerates: same fingerprint, but gone for this scan.
	mu.Lock()
	present = nil
	mu.Unlock()
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "session ended with the device", func() bool { return len(core.Sessions()) == 0 })

	// The consumer must be able to tell "the tool lost the device" from "the
	// device is quiet": a bounded read now reports EOF rather than timing out.
	res := sess.Read(3*time.Second, 256, nil)
	if !res.EOF {
		t.Fatalf("read after the device vanished must report EOF, got %+v", res)
	}
	if res.TimedOut {
		t.Fatal("a vanished device must not present as a timeout; that is what made a broken tool look like a silent device")
	}
}

// countingPort records how many times the hardware was actually opened. On
// Windows the port is opened by CreateFile before any line settings apply, so
// the driver asserts DTR and a board wired for auto-reset reboots. The agent
// cannot prevent that, so the fix is to open once and keep it open: sessions
// attach to the monitor instead of reopening.
func TestMonitorOpensThePortOnce(t *testing.T) {
	store, _ := auth.Open(t.TempDir())
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.HandleWS)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tok, _, _ := store.MintPairingToken()
	agentID, cred, err := store.ExchangePairing(tok, "t")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg, _ := config.Load(dir)
	ports := []serialdev.PortInfo{{Path: "/dev/ttyMON", IsUSB: true, VID: "303a", PID: "1001", SerialNumber: "MON1"}}
	var mu sync.Mutex
	opens := 0
	var last *fakePort
	core := agent.New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { return ports, nil },
		func(string, proto.OpenParams) (serialdev.Port, error) {
			mu.Lock()
			defer mu.Unlock()
			opens++ // each of these is a CreateFile, and so a reset
			last = newFakePort()
			return last, nil
		})
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}
	dev := core.UIDevices()[0]
	if err := core.SetExposed(dev.UUID, true); err != nil {
		t.Fatal(err)
	}

	tun := &agent.Tunnel{Core: core, URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		AgentID: agentID, Credential: cred, InsecureDev: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Run(ctx)
	waitFor(t, "connected", func() bool { s, _ := tun.Status(); return s == "connected" })
	waitFor(t, "announced", func() bool { return len(hub.Devices(nil)) == 1 })

	if err := core.SetMonitored(dev.UUID, true, 115200); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	afterMonitor := opens
	mu.Unlock()
	if afterMonitor != 1 {
		t.Fatalf("starting monitoring should open the port exactly once, got %d", afterMonitor)
	}

	// Three open/close cycles must not touch the hardware again.
	for i := 0; i < 3; i++ {
		s, err := broker.Open(dev.ID, proto.OpenParams{Baud: 115200}, nil, "owner")
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		broker.Close(s.ID)
		waitFor(t, "claim released", func() bool { return len(core.Sessions()) == 0 })
	}
	mu.Lock()
	total := opens
	mu.Unlock()
	if total != 1 {
		t.Fatalf("the port was opened %d times; every open past the first resets the board", total)
	}
}
