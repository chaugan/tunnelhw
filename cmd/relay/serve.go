package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/mcp"
	"github.com/chaugan/tunnelhw/internal/relay"
	"github.com/chaugan/tunnelhw/internal/relayapi"
)

const (
	shutdownTimeout = 10 * time.Second

	// Pairing exchange rate limit (design review: pairing is rate-limited).
	pairLimit  = 10
	pairWindow = time.Minute
)

func runServe(parent context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8443", "address to listen on")
	stateDir := stateDirFlag(fs)
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (PEM); required with --tls-key unless --insecure-dev")
	tlsKey := fs.String("tls-key", "", "TLS private-key file (PEM); required with --tls-cert unless --insecure-dev")
	insecureDev := fs.Bool("insecure-dev", false, "DEVELOPMENT ONLY: serve plain HTTP/ws without TLS")
	mcpEnabled := fs.Bool("mcp", true, "serve the MCP endpoint at /mcp (bearer-authenticated, LLM-host principal)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	useTLS := *tlsCert != "" && *tlsKey != ""
	if (*tlsCert == "") != (*tlsKey == "") {
		return errors.New("--tls-cert and --tls-key must be set together")
	}
	if !useTLS && !*insecureDev {
		return errors.New("refusing to start without TLS: pass --tls-cert/--tls-key, or --insecure-dev for local development only")
	}
	if !useTLS {
		// The docs say plaintext on a non-loopback address is never acceptable,
		// so enforce it rather than warn: without TLS, a reachable address puts
		// agent credentials and device traffic on the wire in clear. Loopback
		// is fine because the SSH carrier (or a local TLS-terminating proxy)
		// supplies the encryption.
		if !loopbackListen(*listen) {
			return fmt.Errorf("refusing to serve plaintext on %q: --insecure-dev is only permitted "+
				"on a loopback address (127.0.0.1 or ::1), where SSH or a local proxy provides the "+
				"encryption.\nEither bind loopback, e.g. --listen 127.0.0.1%s, or serve TLS with "+
				"--tls-cert/--tls-key", *listen, portOf(*listen))
		}
		log.Println("**************************************************************")
		log.Println("* --insecure-dev: serving PLAIN HTTP on loopback only.       *")
		log.Println("* Encryption must come from the SSH carrier or a local       *")
		log.Println("* TLS-terminating proxy. Never expose this port directly.    *")
		log.Println("**************************************************************")
	}

	store, err := auth.Open(*stateDir)
	if err != nil {
		return err
	}
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)
	api := &relayapi.API{Broker: broker, Auth: store}
	mux := newServeMux(hub, api, store, newPairLimiter(pairLimit, pairWindow), *mcpEnabled)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Under a service manager the parent context carries the stop request; in
	// the foreground, Ctrl-C and SIGTERM do.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("relay listening on %s (tls=%v, mcp=%v)", *listen, useTLS, *mcpEnabled)
		if useTLS {
			errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Println("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		// Agent tunnels are long-lived handlers that Shutdown never
		// interrupts; cutting them on SIGTERM is a clean exit, not a failure.
		srv.Close()
		err = nil
	}
	return err
}

// newServeMux builds the relay's full HTTP surface. Split out so tests can
// exercise it over httptest.
func newServeMux(hub *relay.Hub, api *relayapi.API, store *auth.Store, limiter *pairLimiter, mcpEnabled bool) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	m.HandleFunc("/ws", hub.HandleWS)
	m.Handle("/api/v1/", api.Handler())
	m.Handle("POST /pair", pairHandler(store, limiter))

	if mcpEnabled {
		// The MCP handler carries its own bearer-token middleware (LLM-host
		// principal) — strictly separate from agent credentials and never
		// shared with the /ws or /pair paths.
		m.Handle("/mcp", mcp.Handler(api.Broker, func(token string) ([]string, bool, bool) {
			rec, ok := store.VerifyAPIToken(token)
			if !ok {
				return nil, false, false
			}
			return rec.Agents, rec.ReadOnly, true
		}))
	}

	return m
}

// pairLimiter is a fixed-window, in-memory, global rate limiter for the
// pairing endpoint. Pairing is a rare human-driven action, so a coarse global
// cap is enough to blunt token brute-forcing.
type pairLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	count  int
	now    func() time.Time // stubbed in tests
}

func newPairLimiter(limit int, window time.Duration) *pairLimiter {
	return &pairLimiter{limit: limit, window: window, now: time.Now}
}

// allow reports whether one more attempt fits in the current window.
func (l *pairLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.now()
	if n.Sub(l.start) >= l.window {
		l.start, l.count = n, 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}

type pairRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

type pairResponse struct {
	AgentID    string `json:"agent_id"`
	Credential string `json:"credential"`
}

// pairHandler performs the pairing exchange: single-use token in, per-agent
// credential out. Tokens and credentials are never logged.
func pairHandler(store *auth.Store, limiter *pairLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow() {
			pairErr(w, http.StatusTooManyRequests, "too many pairing attempts; try again later")
			return
		}
		var req pairRequest
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Deliberately generic: the body may contain a token.
			pairErr(w, http.StatusBadRequest, "bad request body")
			return
		}
		agentID, credential, err := store.ExchangePairing(req.Token, req.Name)
		if errors.Is(err, auth.ErrBadPairing) {
			pairErr(w, http.StatusForbidden, err.Error())
			return
		}
		if err != nil {
			pairErr(w, http.StatusInternalServerError, "pairing failed")
			return
		}
		log.Printf("paired new agent %s (%q)", agentID, req.Name)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pairResponse{AgentID: agentID, Credential: credential})
	}
}

func pairErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
