// Command agent is the TunnelHW local agent: it enumerates serial hardware,
// serves the localhost-only web UI, and — once paired — dials the relay
// outbound to bridge device sessions (ARCHITECTURE.md §2).
package main

import (
	"context"
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

	"github.com/chaugan/tunnelhw/internal/agent"
	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/serialdev"
	"github.com/chaugan/tunnelhw/internal/svc"
	"github.com/chaugan/tunnelhw/internal/webui"
)

// rescanInterval drives hot-plug detection.
const rescanInterval = 3 * time.Second

const serviceName = "tunnelhw-agent"

// version is set at build time with -X main.version=<tag>.
var version = "dev"

func main() {
	agent.Version = version
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("tunnelhw-agent %s\n", version)
		return
	}
	// "service <action>" manages the background service; anything else runs
	// the agent, in the foreground or under a service manager.
	if len(os.Args) > 2 && os.Args[1] == "service" {
		if err := runServiceCmd(os.Args[2], os.Args[3:]); err != nil {
			log.Fatalf("agent: %v", err)
		}
		return
	}

	cfgDir := flag.String("config-dir", "", "config directory (default: the per-user config dir)")
	listen := flag.String("listen", "", "web UI listen address, loopback only (default: from config, "+config.DefaultUIListen+")")
	insecureDev := flag.Bool("insecure-dev", false, "permit plaintext ws:// relay URLs — development only")
	flag.Usage = usage
	flag.Parse()

	err := svc.Run(spec(nil), func(ctx context.Context) error {
		return run(ctx, *cfgDir, *listen, *insecureDev)
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, `TunnelHW agent — exposes selected local serial hardware to a paired relay.

Usage:
  %s [flags]                     run in the foreground
  %s service install [flags]     install as a background service
  %s service install|start|stop|restart|uninstall|status

Flags:
`, serviceName, serviceName, serviceName)
	flag.PrintDefaults()
}

// spec describes the agent service. args are the flags the installed service
// will run with.
func spec(args []string) svc.Spec {
	return svc.Spec{
		Name:        serviceName,
		DisplayName: "TunnelHW agent",
		Description: "Exposes selected local serial hardware to a paired TunnelHW relay.",
		Arguments:   args,
		System:      systemScope,
	}
}

var systemScope bool

// runServiceCmd handles "service <action> [flags]". Flags given to
// "service install" are recorded and replayed every time the service starts.
func runServiceCmd(action string, rest []string) error {
	fs := flag.NewFlagSet("service "+action, flag.ExitOnError)
	cfgDir := fs.String("config-dir", "", "config directory the service should use")
	listen := fs.String("listen", "", "web UI listen address, loopback only")
	insecureDev := fs.Bool("insecure-dev", false, "permit plaintext ws:// relay URLs — development only")
	system := fs.Bool("system", false, "install system-wide instead of for the current user")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	systemScope = *system

	var args []string
	if *cfgDir != "" {
		args = append(args, "--config-dir", *cfgDir)
	}
	if *listen != "" {
		args = append(args, "--listen", *listen)
	}
	if *insecureDev {
		args = append(args, "--insecure-dev")
	}
	if action == "install" && *cfgDir == "" && (*system || !svc.SupportsUserServices()) {
		// A system service has a different home directory, so the agent would
		// silently use a config dir that is not the one the user set up.
		log.Print("warning: installing a system-scoped service without --config-dir; " +
			"it will use the service account's config directory, not yours")
	}
	return svc.Control(spec(args), action)
}

func run(ctx context.Context, dir, listen string, insecureDev bool) error {
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

	// Under a service manager the caller's context carries the stop signal;
	// in the foreground, Ctrl-C and SIGTERM do.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	tunnels := &tunnelManager{ctx: ctx, core: core}
	if id := core.RelayIdentity(); id.Paired() {
		tunnels.Start(id, insecureDev)
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
func (m *tunnelManager) Start(id agent.RelayIdentity, insecureDev bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	t := &agent.Tunnel{
		Core:        m.core,
		URL:         id.RelayURL,
		AgentID:     id.AgentID,
		Credential:  id.Credential,
		InsecureDev: insecureDev,
		SSH:         id.SSH,
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

// Resume clears the kill switch on the current tunnel.
func (m *tunnelManager) Resume() {
	m.mu.Lock()
	t := m.tun
	m.mu.Unlock()
	if t != nil {
		t.Resume()
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
