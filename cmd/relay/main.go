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
//
// Secrets are printed exactly once at mint time and never logged.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chaugan/tunnelhw/internal/auth"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "pair-token":
		err = runPairToken(args)
	case "api-token":
		err = runAPIToken(args)
	case "agents":
		err = runAgents(args)
	case "revoke-agent":
		err = runRevokeAgent(args)
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
  help                      Show this help.

Run "tunnelhw-relay <command> -h" for command flags.
`)
}

// defaultStateDir is where the credential store lives unless --state-dir
// overrides it.
func defaultStateDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "tunnelhw-relay"
	}
	return filepath.Join(dir, "tunnelhw-relay")
}

// stateDirFlag registers the shared --state-dir flag on a FlagSet.
func stateDirFlag(fs *flag.FlagSet) *string {
	return fs.String("state-dir", defaultStateDir(), "directory holding the relay's credential store")
}

func runPairToken(args []string) error {
	fs := flag.NewFlagSet("pair-token", flag.ExitOnError)
	stateDir := stateDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
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
