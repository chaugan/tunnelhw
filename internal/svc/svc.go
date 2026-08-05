// Package svc runs a TunnelHW binary either in the foreground or under the
// platform's service manager — Windows services, systemd, or launchd — behind
// one set of subcommands.
//
// The important wrinkle is *which account* the service runs as. The agent
// resolves its config directory, `~/.ssh/known_hosts`, `~/.ssh/config` and the
// ssh-agent socket from the user's environment, so a service running as
// LocalSystem or root looks at a different home directory and fails to
// authenticate in ways that are hard to diagnose. Where the platform supports
// per-user services (systemd `--user`, launchd LaunchAgents) that is the
// default; Windows has no such concept, so installing there is a system
// service and the caller is warned to pass explicit paths.
package svc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"
)

// Spec describes a service to install or run.
type Spec struct {
	Name        string   // service identifier, e.g. "tunnelhw-agent"
	DisplayName string   // human-readable name
	Description string   // one-line description
	Arguments   []string // arguments the installed service runs with
	System      bool     // install system-wide rather than per-user
}

// SupportsUserServices reports whether this platform can run a service under
// the invoking user's account.
func SupportsUserServices() bool { return runtime.GOOS != "windows" }

// UserScoped reports whether the spec will run as the invoking user.
func (s Spec) UserScoped() bool { return !s.System && SupportsUserServices() }

func (s Spec) config() *service.Config {
	cfg := &service.Config{
		Name:        s.Name,
		DisplayName: s.DisplayName,
		Description: s.Description,
		Arguments:   s.Arguments,
		Option:      service.KeyValue{},
	}
	if s.UserScoped() {
		cfg.Option["UserService"] = true
		if runtime.GOOS == "linux" {
			cfg.Option["SystemdScript"] = userSystemdScript
		}
	}
	return cfg
}

// userSystemdScript replaces the library's default unit template for
// per-user services. Two things in the default are wrong for a `--user`
// instance: it installs `WantedBy=multi-user.target`, a target that only
// exists in the system instance — so the unit would never start at login —
// and it waits two minutes before restarting, which is a long outage for a
// tunnel that should reconnect promptly.
const userSystemdScript = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
{{range Dependencies}}{{.}}
{{end}}
[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}{{if Restart}}Restart={{Restart}}
{{end}}RestartSec=5

[Install]
WantedBy=default.target
`

// program adapts a context-cancelled run function to the service interface.
// Start must not block, and Stop must return promptly, so the work runs on its
// own goroutine and shutdown is bounded.
type program struct {
	run    func(context.Context) error
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (p *program) Start(service.Service) error {
	go func() {
		defer close(p.done)
		p.err = p.run(p.ctx)
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
	}
	return nil
}

// Run executes fn, either directly (foreground) or under the service manager
// when the process was started by one. fn must return when its context is
// cancelled.
func Run(spec Spec, fn func(context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &program{run: fn, cancel: cancel, done: make(chan struct{}), ctx: ctx}

	s, err := service.New(p, spec.config())
	if err != nil {
		return err
	}
	if err := s.Run(); err != nil {
		return err
	}
	return p.err
}

// unitPaths returns the file the platform's service manager creates for each
// scope, so an already-installed service can be found without guessing.
func unitPaths(name string) (user, system string) {
	home, err := os.UserHomeDir()
	switch runtime.GOOS {
	case "linux":
		if err == nil {
			user = filepath.Join(home, ".config", "systemd", "user", name+".service")
		}
		system = "/etc/systemd/system/" + name + ".service"
	case "darwin":
		if err == nil {
			user = filepath.Join(home, "Library", "LaunchAgents", name+".plist")
		}
		system = "/Library/LaunchDaemons/" + name + ".plist"
	}
	return user, system
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// resolveScope finds the scope an action should act on, and explains any
// change it makes. Requiring --system on every subsequent command is a trap:
// forget it once and the tool reports "not installed" while the service sits
// there installed.
func resolveScope(spec Spec) (Spec, string) {
	if spec.System || !SupportsUserServices() {
		return spec, ""
	}
	// Under sudo the "user" is root, so per-user scope would mean a systemd
	// --user service owned by root — something nobody wants and which does not
	// run without a root login session. Reaching for sudo says "system".
	if os.Getenv("SUDO_USER") != "" {
		spec.System = true
		return spec, "running under sudo, so acting system-wide (pass --system explicitly to silence this)"
	}
	user, system := unitPaths(spec.Name)
	if exists(system) && !exists(user) {
		spec.System = true
		return spec, "acting on the system-scoped service"
	}
	return spec, ""
}

// Control performs an install/uninstall/start/stop/restart/status action.
//
// On Windows these actions need an elevated token, which the process is very
// unlikely to have; rather than fail with "Access is denied" it re-launches
// itself through UAC and reports what the elevated copy did.
func Control(spec Spec, action string) error {
	spec, note := resolveScope(spec)
	if note != "" {
		fmt.Printf("(%s)\n", note)
	}
	if action == "install" {
		// Installing the opposite scope over an existing one fails deep in the
		// service manager with "Init already exists"; say what to do instead.
		user, system := unitPaths(spec.Name)
		if other := map[bool]string{true: user, false: system}[spec.System]; exists(other) {
			return fmt.Errorf("%s is already installed with the other scope (%s)\n"+
				"Remove that one first:  %s%s service uninstall%s",
				spec.Name, other, sudoPrefix(!spec.System), exeName(), scopeFlag(!spec.System))
		}
	} else if user, system := unitPaths(spec.Name); exists(user) && exists(system) {
		// Both scopes installed: acting on one silently would look like the
		// command did nothing to the other.
		fmt.Printf("warning: %s is installed twice — per-user (%s) and system-wide (%s).\n"+
			"         Acting on the %s one.\n",
			spec.Name, user, system, scopeWord(spec))
	}
	if elevationSupported() && needsElevation(action) && !isElevated() {
		fmt.Printf("%s needs Administrator rights — requesting elevation…\n", action)
		code, err := relaunchElevated()
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("elevated %q failed (exit code %d)", action, code)
		}
		// The elevated copy runs in its own console window, which closes
		// immediately, so report the outcome from here where it can be read.
		fmt.Printf("%s: %s completed with Administrator rights\n", spec.Name, action)
		if action == "install" {
			printPostInstallNotes(spec)
		}
		return nil
	}

	s, err := service.New(&program{done: make(chan struct{})}, spec.config())
	if err != nil {
		return err
	}
	switch action {
	case "status":
		st, err := s.Status()
		if err != nil {
			if errors.Is(err, service.ErrNotInstalled) {
				fmt.Println("not installed")
				return nil
			}
			return err
		}
		switch st {
		case service.StatusRunning:
			fmt.Println("running")
		case service.StatusStopped:
			fmt.Println("stopped")
		default:
			fmt.Println("unknown")
		}
		return nil
	case "install":
		if err := service.Control(s, "install"); err != nil {
			return installError(spec, err)
		}
		fmt.Printf("installed %q as a %s service\n", spec.Name, scopeWord(spec))
		printPostInstallNotes(spec)
		return nil
	case "uninstall", "start", "stop", "restart":
		if err := service.Control(s, action); err != nil {
			return err
		}
		fmt.Printf("%s: %sed\n", spec.Name, strings.TrimSuffix(action, "e"))
		return nil
	default:
		return fmt.Errorf("unknown service action %q (want install, uninstall, start, stop, restart, status)", action)
	}
}

// installError adds the context the service manager's bare exit status omits.
// The usual cause on Linux is running a user service from a session with no
// user D-Bus instance — an SSH login without lingering, typically — where
// `systemctl --user` cannot work at all.
func installError(spec Spec, err error) error {
	if spec.UserScoped() && runtime.GOOS == "linux" {
		if os.Getenv("XDG_RUNTIME_DIR") == "" || os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
			return fmt.Errorf("%w\n\nThis session has no user systemd instance (XDG_RUNTIME_DIR or "+
				"DBUS_SESSION_BUS_ADDRESS is unset), so `systemctl --user` cannot run — common over "+
				"plain SSH.\nEither log in on the console, enable lingering with "+
				"`sudo loginctl enable-linger $USER` and reconnect, or install a system service:\n"+
				"  %s service install --system …", err, spec.Name)
		}
	}
	return err
}

// exeName is the invocation to suggest in messages: the real path, since a
// downloaded binary is not on PATH.
func exeName() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "tunnelhw"
}

// sudoPrefix suggests sudo when the suggested command needs system privileges.
func sudoPrefix(system bool) string {
	if system && runtime.GOOS != "windows" {
		return "sudo "
	}
	return ""
}

func scopeFlag(system bool) string {
	if system {
		return " --system"
	}
	return ""
}

func scopeWord(spec Spec) string {
	if spec.UserScoped() {
		return "user"
	}
	return "system"
}

// printPostInstallNotes surfaces the platform facts that otherwise turn into
// confusing runtime failures.
func printPostInstallNotes(spec Spec) {
	exe, _ := os.Executable()
	fmt.Printf("  binary:  %s\n", exe)
	fmt.Printf("  args:    %s\n", strings.Join(spec.Arguments, " "))
	fmt.Println("  NOTE: the service runs this exact path — reinstall if you move the binary.")

	switch {
	case runtime.GOOS == "windows":
		fmt.Println("  NOTE: Windows has no per-user services, so this runs as LocalSystem.")
		fmt.Println("        LocalSystem has a different home directory, so ~/.ssh keys, known_hosts")
		fmt.Println("        and the ssh-agent are NOT the ones you use interactively. Pass explicit")
		fmt.Println("        paths (--config-dir, and an absolute SSH key path in the web UI), or run")
		fmt.Println("        the agent in the foreground instead.")
	case spec.UserScoped() && runtime.GOOS == "linux":
		fmt.Println("  NOTE: this is a systemd --user service; it stops when you log out unless")
		fmt.Printf("        lingering is enabled:  sudo loginctl enable-linger %s\n", os.Getenv("USER"))
	case !spec.UserScoped():
		fmt.Println("  NOTE: a system service runs as root, so ~/.ssh and the per-user config dir")
		fmt.Println("        differ from yours. Pass explicit paths, or install as a user service.")
	}
	if runtime.GOOS == "linux" {
		fmt.Println("  NOTE: serial ports usually require group membership:  sudo usermod -aG dialout $USER")
	}
	// Print the path actually used, not the bare service name: the binary is
	// rarely on PATH, and a system service needs the same privilege to start
	// as it did to install.
	start := exe + " service start"
	if !spec.UserScoped() && runtime.GOOS != "windows" {
		start = "sudo " + start
	}
	fmt.Printf("  start it with:  %s\n", start)
}
