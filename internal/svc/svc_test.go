package svc

import (
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
		t.Fatal("user unit must not reference multi-user.target — it does not exist in a --user instance")
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
