// Command tunnelhw-relay is the relay server: it terminates agent tunnels,
// serves the versioned relay API, and hosts the admin CLI for pairing and
// token management.
//
// Subcommands:
//
//	serve                 run the relay server (default when no subcommand)
//	pair-token            mint a single-use agent pairing token
//	api-token             mint a consumer (LLM-host / API) bearer token
//	agents                list paired agents
//	revoke-agent <id>     revoke an agent's credential
//	service <action>      install/uninstall/start/stop/restart/status
//
// Secrets are printed exactly once at mint time and never logged.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/mcp"
	"github.com/chaugan/tunnelhw/internal/svc"
)

// version is set at build time with -X main.version=<tag>.
var version = "dev"

func main() {
	mcp.Version = version
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		// Run through the service wrapper so the same code path works in the
		// foreground and under a service manager.
		err = svc.Run(spec(nil), func(ctx context.Context) error { return runServe(ctx, args) })
	case "pair-token":
		err = runPairToken(args)
	case "api-token":
		err = runAPIToken(args)
	case "agents":
		err = runAgents(args)
	case "revoke-agent":
		err = runRevokeAgent(args)
	case "service":
		if len(args) == 0 {
			err = fmt.Errorf("service needs an action: install, uninstall, start, stop, restart, status")
			break
		}
		err = runServiceCmd(args[0], args[1:])
	case "version":
		fmt.Printf("tunnelhw-relay %s\n", version)
	case "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tunnelhw-relay:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `Usage: tunnelhw-relay [command] [flags]

Commands:
  serve                     Run the relay server (default).
  pair-token                Mint a single-use agent pairing token.
  api-token                 Mint a consumer API bearer token.
  agents                    List paired agents.
  revoke-agent <agent-id>   Revoke an agent's credential.
  service <action>          install | uninstall | start | stop | restart | status
  version                   Print the build version.
  help                      Show this help.

Run "tunnelhw-relay <command> -h" for command flags.

"service install" takes the same flags as "serve" and records them, so the
service starts with exactly that configuration. It installs for the current
user where the platform supports it; use --system for a system-wide service.
`)
}

// systemContext reports whether we are acting on behalf of the machine rather
// than the invoking user: running as root, via sudo, or as a Windows service.
// The credential store has to follow that distinction, because a relay
// installed as a system service and the admin commands run with sudo must
// land on the *same* store — otherwise tokens are minted where the service
// will never look for them.
func systemContext() bool {
	if os.Getenv("SUDO_USER") != "" {
		return true
	}
	if runtime.GOOS != "windows" {
		return os.Geteuid() == 0
	}
	// A Windows service runs as LocalSystem, whose profile is under
	// %WINDIR%\System32\config\systemprofile.
	if cfg, err := os.UserConfigDir(); err == nil {
		return strings.Contains(strings.ToLower(cfg), `system32\config\systemprofile`)
	}
	return false
}

// systemStateDir is the machine-wide location for the credential store.
func systemStateDir() string {
	switch runtime.GOOS {
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "tunnelhw-relay")
		}
		return `C:\ProgramData\tunnelhw-relay`
	case "darwin":
		return "/Library/Application Support/tunnelhw-relay"
	default:
		return "/var/lib/tunnelhw-relay"
	}
}

// userStateDir is the per-user location for the credential store.
func userStateDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "tunnelhw-relay"
	}
	return filepath.Join(dir, "tunnelhw-relay")
}

// defaultStateDir is where the credential store lives unless --state-dir
// overrides it: machine-wide when acting for the machine, per-user otherwise.
func defaultStateDir() string {
	if systemContext() {
		return systemStateDir()
	}
	return userStateDir()
}

// stateDirFlag registers the shared --state-dir flag on a FlagSet.
func stateDirFlag(fs *flag.FlagSet) *string {
	return fs.String("state-dir", defaultStateDir(), "directory holding the relay's credential store")
}

// reportStateDir names the store being used, and points at the other one when
// this looks like the mistake that costs an afternoon: minting into an empty
// store while the credentials sit in the other scope's.
func reportStateDir(dir string) {
	fmt.Fprintf(os.Stderr, "using credential store: %s\n", dir)
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); err == nil {
		return
	}
	other := userStateDir()
	if dir == other {
		other = systemStateDir()
	}
	if _, err := os.Stat(filepath.Join(other, "auth.json")); err == nil {
		fmt.Fprintf(os.Stderr,
			"note: this store is empty, but one exists at %s.\n"+
				"      If that is the one your relay uses, add --state-dir %s\n",
			other, other)
	}
}

func runPairToken(args []string) error {
	fs := flag.NewFlagSet("pair-token", flag.ExitOnError)
	stateDir := stateDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	reportStateDir(*stateDir)
	store, err := auth.Open(*stateDir)
	if err != nil {
		return err
	}
	tok, exp, err := store.MintPairingToken()
	if err != nil {
		return err
	}
	fmt.Printf("Pairing token (single use, shown once — paste it into the agent's web UI):\n\n  %s\n\nExpires: %s (in %s)\n",
		tok, exp.Format(time.RFC3339), time.Until(exp).Round(time.Second))
	return nil
}

func runAPIToken(args []string) error {
	fs := flag.NewFlagSet("api-token", flag.ExitOnError)
	stateDir := stateDirFlag(fs)
	name := fs.String("name", "", "human-readable label for the token")
	readOnly := fs.Bool("read-only", false, "token may list/read but not open, write, or change params")
	agentsCSV := fs.String("agents", "", "comma-separated agent IDs the token is scoped to (empty = all agents)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	reportStateDir(*stateDir)
	var agents []string
	for _, a := range strings.Split(*agentsCSV, ",") {
		if a = strings.TrimSpace(a); a != "" {
			agents = append(agents, a)
		}
	}
	store, err := auth.Open(*stateDir)
	if err != nil {
		return err
	}
	tok, err := store.MintAPIToken(*name, *readOnly, agents)
	if err != nil {
		return err
	}
	fmt.Printf("API token (shown once — store it now):\n\n  %s\n\n", tok)
	if *name != "" {
		fmt.Printf("Name:      %s\n", *name)
	}
	fmt.Printf("Read-only: %v\n", *readOnly)
	if len(agents) > 0 {
		fmt.Printf("Agents:    %s\n", strings.Join(agents, ", "))
	} else {
		fmt.Println("Agents:    all")
	}
	return nil
}

func runAgents(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	stateDir := stateDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	reportStateDir(*stateDir)
	store, err := auth.Open(*stateDir)
	if err != nil {
		return err
	}
	recs := store.Agents()
	ids := make([]string, 0, len(recs))
	for id := range recs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT ID\tNAME\tPAIRED AT\tREVOKED")
	for _, id := range ids {
		rec := recs[id]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n", id, rec.Name, rec.PairedAt.Format(time.RFC3339), rec.Revoked)
	}
	return tw.Flush()
}

func runRevokeAgent(args []string) error {
	fs := flag.NewFlagSet("revoke-agent", flag.ExitOnError)
	stateDir := stateDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	reportStateDir(*stateDir)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: tunnelhw-relay revoke-agent [--state-dir dir] <agent-id>")
	}
	store, err := auth.Open(*stateDir)
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := store.RevokeAgent(id); err != nil {
		return err
	}
	fmt.Printf("Revoked agent %s. Its next connection attempt will be rejected.\n", id)
	return nil
}
