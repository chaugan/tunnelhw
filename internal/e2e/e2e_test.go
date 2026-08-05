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
