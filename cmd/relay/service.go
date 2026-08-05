package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/chaugan/tunnelhw/internal/svc"
)

const serviceName = "tunnelhw-relay"

var systemScope bool

// spec describes the relay service. args are the arguments the installed
// service replays on every start — always beginning with "serve".
func spec(args []string) svc.Spec {
	return svc.Spec{
		Name:        serviceName,
		DisplayName: "TunnelHW relay",
		Description: "Terminates TunnelHW agent tunnels and serves the relay API and MCP endpoint.",
		Arguments:   args,
		System:      systemScope,
	}
}

// runServiceCmd handles "service <action> [flags]". Flags accepted by
// "service install" mirror those of "serve" and are recorded into the service
// definition, so the service starts with exactly the configuration you asked
// for.
func runServiceCmd(action string, rest []string) error {
	fs := flag.NewFlagSet("service "+action, flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on (default :8443)")
	stateDir := fs.String("state-dir", "", "directory holding auth.json")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := fs.String("tls-key", "", "TLS private-key file (PEM)")
	insecureDev := fs.Bool("insecure-dev", false, "serve without TLS — only safe on loopback behind SSH")
	mcp := fs.Bool("mcp", true, "serve the MCP endpoint")
	system := fs.Bool("system", false, "install system-wide instead of for the current user")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	systemScope = *system

	// Pin the store into the unit rather than leaving it to a runtime default:
	// the service and the admin commands must agree on one location, and the
	// unit file should say which.
	resolvedState := *stateDir
	if resolvedState == "" {
		if *system || !svc.SupportsUserServices() {
			resolvedState = systemStateDir()
		} else {
			resolvedState = userStateDir()
		}
	}

	args := []string{"serve"}
	add := func(flagName, v string) {
		if v != "" {
			args = append(args, "--"+flagName, v)
		}
	}
	add("listen", *listen)
	add("state-dir", resolvedState)
	add("tls-cert", *tlsCert)
	add("tls-key", *tlsKey)
	if *insecureDev {
		args = append(args, "--insecure-dev")
	}
	if !*mcp {
		args = append(args, "--mcp=false")
	}

	if action == "install" {
		fmt.Printf("credential store: %s\n", resolvedState)
		// Credentials minted before the store moved would otherwise be
		// invisible to the new service, which looks exactly like a broken
		// pairing.
		if _, err := os.Stat(filepath.Join(resolvedState, "auth.json")); err != nil {
			for _, other := range []string{userStateDir(), systemStateDir()} {
				if other == resolvedState {
					continue
				}
				if _, err := os.Stat(filepath.Join(other, "auth.json")); err == nil {
					fmt.Printf("note: an existing store was found at %s.\n"+
						"      To keep current pairings and tokens, copy it across before starting:\n"+
						"        sudo mkdir -p %s && sudo cp %s %s && sudo chmod 600 %s\n",
						other, resolvedState,
						filepath.Join(other, "auth.json"), resolvedState,
						filepath.Join(resolvedState, "auth.json"))
					break
				}
			}
		}
		if *insecureDev && !loopbackListen(*listen) {
			log.Print("WARNING: --insecure-dev with a non-loopback listen address serves " +
				"agent credentials and device traffic in cleartext; use --tls-cert/--tls-key instead")
		}
	}
	return svc.Control(spec(args), action)
}

// loopbackListen reports whether addr is unambiguously loopback-only.
func loopbackListen(addr string) bool {
	switch {
	case addr == "":
		return false // default :8443 binds every interface
	case len(addr) > 0 && addr[0] == ':':
		return false
	}
	host, _, err := splitHostPortLoose(addr)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func splitHostPortLoose(addr string) (host, port string, err error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", nil
}

// portOf returns the ":port" suffix of a listen address, for use in messages
// that suggest a corrected address.
func portOf(addr string) string {
	if _, port, err := splitHostPortLoose(addr); err == nil && port != "" {
		return ":" + port
	}
	return ":8443"
}
