// Package webui serves the agent's localhost-only web UI and JSON API with
// the full hardening set from ARCHITECTURE.md §7: loopback-only listener,
// Host-header allowlist, Origin rejection, per-launch CSRF secret, strict
// Content-Security-Policy, and no CORS headers at all.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chaugan/tunnelhw/internal/agent"
	"github.com/chaugan/tunnelhw/web"
)

// pairTimeout bounds the pairing-token exchange with the relay.
const pairTimeout = 15 * time.Second

// csrfPlaceholder is replaced in index.html with the per-launch secret.
const csrfPlaceholder = "__CSRF_TOKEN__"

// TunnelController lets the UI start, replace, and sever the relay tunnel at
// runtime (pairing, kill switch). The agent binary provides the real one.
type TunnelController interface {
	// Start (re)starts the tunnel with fresh settings, replacing any
	// running one.
	Start(relayURL, agentID, credential string, insecureDev bool)
	// Disconnect severs the current tunnel session (kill switch).
	Disconnect()
	// Status reports the tunnel state and last error for the UI.
	Status() (state, lastErr string)
}

// Server is the localhost web UI. Construct with New; serve Handler on a
// listener from Listen. All persisted state goes through Core, which is the
// config's single owner.
type Server struct {
	core        *agent.Core
	tc          TunnelController
	insecureDev bool

	boundHost     string // e.g. "127.0.0.1:8787" — the only allowed Host
	localhostHost string // "localhost:<port>" — also allowed
	secret        string // per-launch CSRF secret
	index         []byte // index.html with the secret injected

	pairClient *http.Client
}

// Listen binds the UI listener, refusing any non-loopback address. The host
// must be a loopback IP literal — not "localhost" — so the bind never goes
// dual-stack or non-local (ARCHITECTURE.md §7).
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("webui: listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("webui: refusing non-loopback listen address %q — the UI is localhost-only", addr)
	}
	return net.Listen("tcp", addr)
}

// New builds the UI server. boundAddr is the address the listener actually
// bound (net.Listener.Addr().String()) — it anchors the Host/Origin
// allowlist.
func New(core *agent.Core, tc TunnelController, boundAddr string, insecureDev bool) (*Server, error) {
	if core == nil || tc == nil {
		return nil, errors.New("webui: core and tunnel controller are required")
	}
	_, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return nil, fmt.Errorf("webui: bound address %q: %w", boundAddr, err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("webui: session secret: %w", err)
	}
	secret := hex.EncodeToString(buf)
	raw, err := web.FS.ReadFile("index.html")
	if err != nil {
		return nil, fmt.Errorf("webui: embedded index.html: %w", err)
	}
	return &Server{
		core:          core,
		tc:            tc,
		insecureDev:   insecureDev,
		boundHost:     boundAddr,
		localhostHost: "localhost:" + port,
		secret:        secret,
		index:         bytes.ReplaceAll(raw, []byte(csrfPlaceholder), []byte(secret)),
		pairClient:    &http.Client{Timeout: pairTimeout},
	}, nil
}

// Handler returns the fully hardened HTTP handler.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", s.serveIndex)
	m.HandleFunc("GET /app.js", s.serveAsset("app.js", "text/javascript; charset=utf-8"))
	m.HandleFunc("GET /style.css", s.serveAsset("style.css", "text/css; charset=utf-8"))
	m.HandleFunc("GET /api/status", s.status)
	m.HandleFunc("GET /api/devices", s.devices)
	m.HandleFunc("POST /api/devices/{uuid}", s.deviceToggle)
	m.HandleFunc("POST /api/devices/{uuid}/regenerate", s.regenerate)
	m.HandleFunc("GET /api/activity", s.activity)
	m.HandleFunc("GET /api/sessions", s.sessions)
	m.HandleFunc("POST /api/pair", s.pair)
	m.HandleFunc("POST /api/disconnect", s.disconnect)
	return s.secure(m)
}

// secure is the §7 middleware: Host allowlist, Origin rejection, CSRF on
// every state-changing request, strict CSP, and deliberately no CORS headers.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden: unrecognized Host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !s.hostAllowed(u.Host) {
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tok := r.Header.Get("X-TunnelHW-CSRF")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.secret)) != 1 {
				http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed permits exactly the bound address and localhost:<port> —
// anything else (DNS-rebinding hostnames included) is rejected.
func (s *Server) hostAllowed(host string) bool {
	return host == s.boundHost || host == s.localhostHost
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.index)
}

func (s *Server) serveAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := web.FS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(raw)
	}
}

// ---- JSON API ------------------------------------------------------------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	relayURL, agentID, cred := s.core.RelayIdentity()
	paired := relayURL != "" && agentID != "" && cred != ""
	state, lastErr := s.tc.Status()
	jsonOut(w, http.StatusOK, map[string]any{
		"paired":       paired,
		"relay_url":    relayURL,
		"tunnel_state": state,
		"tunnel_error": lastErr,
	})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	devs := s.core.UIDevices()
	if devs == nil {
		devs = []agent.UIDevice{}
	}
	jsonOut(w, http.StatusOK, devs)
}

func (s *Server) deviceToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Exposed           *bool `json:"exposed"`
		AllowControlLines *bool `json:"allow_control_lines"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	id := r.PathValue("uuid")
	if req.Exposed != nil {
		if err := s.core.SetExposed(id, *req.Exposed); err != nil {
			jsonErr(w, http.StatusNotFound, err.Error())
			return
		}
	}
	if req.AllowControlLines != nil {
		if err := s.core.SetControlLines(id, *req.AllowControlLines); err != nil {
			jsonErr(w, http.StatusNotFound, err.Error())
			return
		}
	}
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) regenerate(w http.ResponseWriter, r *http.Request) {
	if err := s.core.RegenerateName(r.PathValue("uuid")); err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	entries := s.core.Activity().Since(after)
	if entries == nil {
		entries = []agent.Entry{}
	}
	jsonOut(w, http.StatusOK, entries)
}

// sessionView is the UI's live-session row.
type sessionView struct {
	SessionID  string    `json:"session_id"`
	DeviceID   string    `json:"device_id"`
	DeviceUUID string    `json:"device_uuid"`
	Opened     time.Time `json:"opened"`
	BytesIn    uint64    `json:"bytes_in"`
	BytesOut   uint64    `json:"bytes_out"`
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	out := []sessionView{}
	for _, sess := range s.core.Sessions() {
		in, outBytes := sess.Counters()
		out = append(out, sessionView{
			SessionID:  sess.ID,
			DeviceID:   sess.DeviceID,
			DeviceUUID: sess.DeviceUUID,
			Opened:     sess.Opened,
			BytesIn:    in,
			BytesOut:   outBytes,
		})
	}
	jsonOut(w, http.StatusOK, out)
}

// pair exchanges a pairing token with the relay over verified TLS, persists
// the minted identity, and (re)starts the tunnel. The token and credential
// are never logged (ARCHITECTURE.md §7).
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RelayURL string `json:"relay_url"`
		Token    string `json:"token"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if req.RelayURL == "" || req.Token == "" {
		jsonErr(w, http.StatusBadRequest, "relay_url and token are required")
		return
	}
	pairBase, tunnelURL, err := normalizeRelayURL(req.RelayURL, s.insecureDev)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	body, err := json.Marshal(map[string]string{"token": req.Token, "name": req.Name})
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pairTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, pairBase+"/pair", bytes.NewReader(body))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.pairClient.Do(httpReq)
	if err != nil {
		// The error string carries the URL, never the token (it's in the body).
		jsonErr(w, http.StatusBadGateway, "relay unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		jsonErr(w, http.StatusBadGateway, fmt.Sprintf("relay refused pairing (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg))))
		return
	}
	var minted struct {
		AgentID    string `json:"agent_id"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&minted); err != nil || minted.AgentID == "" || minted.Credential == "" {
		jsonErr(w, http.StatusBadGateway, "relay returned an invalid pairing response")
		return
	}

	if err := s.core.SetRelayIdentity(tunnelURL, minted.AgentID, minted.Credential); err != nil {
		jsonErr(w, http.StatusInternalServerError, "persist pairing: "+err.Error())
		return
	}

	s.core.Activity().Add("tunnel", "paired with relay "+pairBase)
	s.tc.Start(tunnelURL, minted.AgentID, minted.Credential, s.insecureDev)
	jsonOut(w, http.StatusOK, map[string]any{"ok": true, "relay_url": tunnelURL})
}

// disconnect is the kill switch: sever the tunnel and every device session.
func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	s.core.Activity().Add("tunnel", "kill switch: severing relay link and all sessions")
	s.tc.Disconnect()
	s.core.CloseAll("kill switch")
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

// normalizeRelayURL turns a user-entered relay URL (wss://host[:port][/path]
// or https://...) into the https:// base for the pairing exchange and the
// ws(s):// URL the tunnel dials. Plaintext schemes need insecure-dev.
func normalizeRelayURL(raw string, insecureDev bool) (pairBase, tunnelURL string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("relay URL: %w", err)
	}
	if u.Host == "" {
		return "", "", errors.New("relay URL must include a host, e.g. wss://relay.example.com")
	}
	var httpScheme, wsScheme string
	switch u.Scheme {
	case "wss", "https":
		httpScheme, wsScheme = "https", "wss"
	case "ws", "http":
		if !insecureDev {
			return "", "", errors.New("plaintext relay URL — TLS is required unless the agent runs with --insecure-dev")
		}
		httpScheme, wsScheme = "http", "ws"
	default:
		return "", "", fmt.Errorf("relay URL scheme %q: want wss:// or https://", u.Scheme)
	}
	// A relay behind a reverse-proxy path prefix mounts /pair and /ws under
	// that prefix — derive both URLs from it. Accepted inputs:
	// wss://host, wss://host/ws, wss://host/prefix, wss://host/prefix/ws.
	prefix := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/ws")
	prefix = strings.TrimSuffix(prefix, "/")
	return httpScheme + "://" + u.Host + prefix, wsScheme + "://" + u.Host + prefix + "/ws", nil
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}
