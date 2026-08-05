// Command agent is the TunnelHW local agent: it enumerates serial hardware,
// serves the localhost-only web UI, and — once paired — dials the relay
// outbound to bridge device sessions (ARCHITECTURE.md §2).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/chaugan/tunnelhw/internal/agent"
	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/serialdev"
	"github.com/chaugan/tunnelhw/internal/webui"
)

// rescanInterval drives hot-plug detection.
const rescanInterval = 3 * time.Second

func main() {
	cfgDir := flag.String("config-dir", "", "config directory (default: the per-user config dir)")
	listen := flag.String("listen", "", "web UI listen address, loopback only (default: from config, "+config.DefaultUIListen+")")
	insecureDev := flag.Bool("insecure-dev", false, "permit plaintext ws:// relay URLs — development only")
	flag.Parse()

	if err := run(*cfgDir, *listen, *insecureDev); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

func run(dir, listen string, insecureDev bool) error {
	var err error
	if dir == "" {
		if dir, err = config.Dir(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	if listen != "" {
		cfg.UIListen = listen
	}

	core := agent.New(dir, cfg, serialdev.Enumerate, serialdev.Open)
	if err := core.Rescan(); err != nil {
		// Not fatal: the periodic rescan keeps retrying.
		log.Printf("initial device scan: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tunnels := &tunnelManager{ctx: ctx, core: core}
	if relayURL, agentID, cred := core.RelayIdentity(); relayURL != "" && agentID != "" && cred != "" {
		tunnels.Start(relayURL, agentID, cred, insecureDev)
	}

	ln, err := webui.Listen(cfg.UIListen)
	if err != nil {
		return err
	}
	ui, err := webui.New(core, tunnels, ln.Addr().String(), insecureDev)
	if err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: ui.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()
	log.Printf("web UI listening on http://%s", ln.Addr())

	rescan := time.NewTicker(rescanInterval)
	defer rescan.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			core.CloseAll("agent shutting down")
			shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := srv.Shutdown(shutCtx)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				// A slow in-flight handler (e.g. a pairing exchange) is not a
				// failed shutdown; cut it and exit clean.
				srv.Close()
				err = nil
			}
			return err
		case err := <-srvErr:
			return err
		case <-rescan.C:
			if err := core.Rescan(); err != nil {
				log.Printf("rescan: %v", err)
			}
		}
	}
}

// tunnelManager owns the tunnel goroutine's lifecycle so pairing can replace
// it at runtime. It implements webui.TunnelController.
type tunnelManager struct {
	ctx  context.Context
	core *agent.Core

	mu     sync.Mutex
	tun    *agent.Tunnel
	cancel context.CancelFunc
}

// Start replaces any running tunnel with one using the given settings.
func (m *tunnelManager) Start(relayURL, agentID, credential string, insecureDev bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	t := &agent.Tunnel{
		Core:        m.core,
		URL:         relayURL,
		AgentID:     agentID,
		Credential:  credential,
		InsecureDev: insecureDev,
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.tun, m.cancel = t, cancel
	go t.Run(ctx)
}

// Disconnect severs the current tunnel session (kill switch); the run loop
// keeps retrying unless the agent is shutting down.
func (m *tunnelManager) Disconnect() {
	m.mu.Lock()
	t := m.tun
	m.mu.Unlock()
	if t != nil {
		t.Disconnect()
	}
}

// Status reports the current tunnel state for the UI.
func (m *tunnelManager) Status() (string, string) {
	m.mu.Lock()
	t := m.tun
	m.mu.Unlock()
	if t == nil {
		return "disconnected", ""
	}
	return t.Status()
}
