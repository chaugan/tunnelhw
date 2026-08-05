package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chaugan/tunnelhw/internal/agent"
	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/serialdev"
)

type fakeTunnel struct {
	started      bool
	disconnected bool
}

func (f *fakeTunnel) Start(relayURL, agentID, credential string, insecureDev bool) { f.started = true }
func (f *fakeTunnel) Disconnect()                                                 { f.disconnected = true }
func (f *fakeTunnel) Status() (string, string)                                    { return "disconnected", "" }

func newTestServer(t *testing.T) (*Server, *fakeTunnel) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Devices: map[string]config.DeviceRecord{}, UIListen: config.DefaultUIListen}
	core := agent.New(dir, cfg, func() ([]serialdev.PortInfo, error) { return nil, nil }, nil)
	ft := &fakeTunnel{}
	s, err := New(core, ft, "127.0.0.1:8787", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, ft
}

func TestSecurityMiddleware(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	cases := []struct {
		name   string
		method string
		path   string
		host   string
		origin string
		csrf   string
		want   int
	}{
		{"bound host allowed", "GET", "/api/status", "127.0.0.1:8787", "", "", 200},
		{"localhost host allowed", "GET", "/api/status", "localhost:8787", "", "", 200},
		{"foreign host rejected", "GET", "/api/status", "evil.example:8787", "", "", 403},
		{"rebinding-style host rejected", "GET", "/api/status", "127.0.0.1.evil.example:8787", "", "", 403},
		{"wrong port rejected", "GET", "/api/status", "127.0.0.1:9999", "", "", 403},
		{"good origin allowed", "GET", "/api/status", "127.0.0.1:8787", "http://127.0.0.1:8787", "", 200},
		{"localhost origin allowed", "GET", "/api/status", "127.0.0.1:8787", "http://localhost:8787", "", 200},
		{"foreign origin rejected", "GET", "/api/status", "127.0.0.1:8787", "https://evil.example", "", 403},
		{"null origin rejected", "GET", "/api/status", "127.0.0.1:8787", "null", "", 403},
		{"post without csrf rejected", "POST", "/api/disconnect", "127.0.0.1:8787", "", "", 403},
		{"post with wrong csrf rejected", "POST", "/api/disconnect", "127.0.0.1:8787", "", "not-the-secret", 403},
		{"post with csrf allowed", "POST", "/api/disconnect", "127.0.0.1:8787", "", s.secret, 200},
		{"good origin cannot skip csrf", "POST", "/api/disconnect", "127.0.0.1:8787", "http://127.0.0.1:8787", "", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, strings.NewReader("{}"))
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.csrf != "" {
				r.Header.Set("X-TunnelHW-CSRF", tc.csrf)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
			if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
				t.Fatalf("CSP header = %q", got)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("unexpected CORS header %q", got)
			}
		})
	}
}

func TestKillSwitchHitsController(t *testing.T) {
	s, ft := newTestServer(t)
	h := s.Handler()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/disconnect", nil)
	r.Header.Set("X-TunnelHW-CSRF", s.secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}
	if !ft.disconnected {
		t.Fatal("controller.Disconnect was not called")
	}
}

func TestIndexInjectsSecretNotPlaceholder(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("index: %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, csrfPlaceholder) {
		t.Fatal("index still contains the CSRF placeholder")
	}
	if !strings.Contains(body, s.secret) {
		t.Fatal("index does not carry the session secret")
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.0.2.1:0", "localhost:0", "8787", "[::]:0"} {
		if ln, err := Listen(addr); err == nil {
			ln.Close()
			t.Fatalf("Listen(%q) accepted a non-loopback (or non-literal) address", addr)
		}
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen loopback: %v", err)
	}
	ln.Close()
}

func TestNormalizeRelayURL(t *testing.T) {
	cases := []struct {
		in       string
		insecure bool
		base     string
		tunnel   string
		wantErr  bool
	}{
		{"wss://relay.example.com", false, "https://relay.example.com", "wss://relay.example.com/ws", false},
		{"https://relay.example.com:8443", false, "https://relay.example.com:8443", "wss://relay.example.com:8443/ws", false},
		// A path is a reverse-proxy mount prefix: /pair and /ws live under it.
		{"wss://relay.example.com/custom", false, "https://relay.example.com/custom", "wss://relay.example.com/custom/ws", false},
		{"wss://relay.example.com/custom/ws", false, "https://relay.example.com/custom", "wss://relay.example.com/custom/ws", false},
		{"wss://relay.example.com/ws", false, "https://relay.example.com", "wss://relay.example.com/ws", false},
		{"ws://127.0.0.1:9000", false, "", "", true},
		{"ws://127.0.0.1:9000", true, "http://127.0.0.1:9000", "ws://127.0.0.1:9000/ws", false},
		{"ftp://x", false, "", "", true},
		{"relay.example.com", false, "", "", true},
	}
	for _, tc := range cases {
		base, tunnel, err := normalizeRelayURL(tc.in, tc.insecure)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("normalizeRelayURL(%q): want error, got %q %q", tc.in, base, tunnel)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeRelayURL(%q): %v", tc.in, err)
		}
		if base != tc.base || tunnel != tc.tunnel {
			t.Fatalf("normalizeRelayURL(%q) = %q, %q; want %q, %q", tc.in, base, tunnel, tc.base, tc.tunnel)
		}
	}
}
