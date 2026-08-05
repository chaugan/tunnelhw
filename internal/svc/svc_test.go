package svc

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestUserScoped(t *testing.T) {
	cases := []struct {
		name   string
		spec   Spec
		want   bool
		reason string
	}{
		{"default", Spec{Name: "x"}, SupportsUserServices(), "per-user where the platform allows it"},
		{"forced system", Spec{Name: "x", System: true}, false, "--system always means system scope"},
	}
	for _, c := range cases {
		if got := c.spec.UserScoped(); got != c.want {
			t.Errorf("%s: UserScoped() = %v, want %v (%s)", c.name, got, c.want, c.reason)
		}
	}
	if runtime.GOOS == "windows" && SupportsUserServices() {
		t.Error("Windows has no per-user services; SupportsUserServices must be false there")
	}
}

func TestConfigScopeOptions(t *testing.T) {
	sys := Spec{Name: "x", System: true}.config()
	if _, ok := sys.Option["UserService"]; ok {
		t.Error("a system service must not set UserService")
	}
	if _, ok := sys.Option["SystemdScript"]; ok {
		t.Error("a system service must use the library's default unit template")
	}

	user := Spec{Name: "x"}.config()
	if SupportsUserServices() {
		if user.Option["UserService"] != true {
			t.Error("user-scoped service must set UserService")
		}
	}
}

// A user unit installed with WantedBy=multi-user.target never starts: that
// target exists only in the system instance. This guards the override.
func TestUserSystemdUnitTargetsDefaultTarget(t *testing.T) {
	if strings.Contains(userSystemdScript, "multi-user.target") {
		t.Fatal("user unit must not reference multi-user.target: it does not exist in a --user instance")
	}
	if !strings.Contains(userSystemdScript, "WantedBy=default.target") {
		t.Fatal("user unit must be WantedBy=default.target so it starts at login")
	}
	if strings.Contains(userSystemdScript, "RestartSec=120") {
		t.Fatal("a two-minute restart delay is too long for a tunnel that should reconnect promptly")
	}
	for _, required := range []string{"ExecStart=", "ConditionFileIsExecutable=", "Description="} {
		if !strings.Contains(userSystemdScript, required) {
			t.Errorf("user unit template is missing %q", required)
		}
	}
}

func TestControlRejectsUnknownAction(t *testing.T) {
	err := Control(Spec{Name: "tunnelhw-test-nonexistent"}, "frobnicate")
	if err == nil || !strings.Contains(err.Error(), "unknown service action") {
		t.Fatalf("err = %v, want an unknown-action error", err)
	}
}

// Forgetting --system on a follow-up command must not report "not installed"
// while the service is sitting there installed.
func TestResolveScopeFindsSystemService(t *testing.T) {
	if !SupportsUserServices() {
		t.Skip("no user services on this platform")
	}
	user, system := unitPaths("tunnelhw-scope-probe")
	if system == "" {
		t.Skip("no unit paths known for this platform")
	}
	if user == system {
		t.Fatal("user and system unit paths must differ")
	}

	// Explicit --system is always honoured.
	if got, note := resolveScope(Spec{Name: "x", System: true}); !got.System || note != "" {
		t.Errorf("explicit --system: got System=%v note=%q", got.System, note)
	}
	// With neither installed, stay user-scoped.
	if os.Getenv("SUDO_USER") == "" {
		if got, note := resolveScope(Spec{Name: "tunnelhw-definitely-not-installed"}); got.System || note != "" {
			t.Errorf("nothing installed: got System=%v note=%q", got.System, note)
		}
	}
}

func TestUnitPathsAreScopeDistinct(t *testing.T) {
	user, system := unitPaths("tunnelhw-relay")
	switch runtime.GOOS {
	case "linux":
		if system != "/etc/systemd/system/tunnelhw-relay.service" {
			t.Errorf("system unit path = %q", system)
		}
		if user != "" && !strings.HasSuffix(user, "/.config/systemd/user/tunnelhw-relay.service") {
			t.Errorf("user unit path = %q", user)
		}
	case "darwin":
		if system != "/Library/LaunchDaemons/tunnelhw-relay.plist" {
			t.Errorf("system plist path = %q", system)
		}
	}
}

// Reaching for sudo means "system"; a systemd --user service owned by root
// does not run and is never what the operator wanted.
func TestSudoImpliesSystemScope(t *testing.T) {
	if !SupportsUserServices() {
		t.Skip("no user services on this platform")
	}
	t.Setenv("SUDO_USER", "chrzz")
	got, note := resolveScope(Spec{Name: "tunnelhw-relay"})
	if !got.System {
		t.Fatal("under sudo the scope must resolve to system")
	}
	if note == "" {
		t.Error("the scope change must be explained, not silent")
	}
	// An explicit flag still wins outright.
	if got, _ := resolveScope(Spec{Name: "x", System: true}); !got.System {
		t.Error("explicit --system must remain system")
	}
}
