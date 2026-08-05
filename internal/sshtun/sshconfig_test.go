package sshtun

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLookupSSHConfig(t *testing.T) {
	path := writeConfig(t, `
# a comment
Host bastion
    HostName bastion.example.com
    User ops
    Port 2222
    IdentityFile ~/.ssh/bastion_key

Host *.internal prod-*
    User deploy
    IdentityFile ~/.ssh/deploy_key

Host *
    IdentityFile ~/.ssh/id_fallback
`)

	got := lookupSSHConfigIn(path, "bastion")
	if got.HostName != "bastion.example.com" || got.User != "ops" || got.Port != "2222" {
		t.Fatalf("bastion = %+v", got)
	}
	// First match wins for single-valued keys, but IdentityFile accumulates
	// across matching blocks; the "Host *" fallback must also be offered.
	if len(got.IdentityFiles) != 2 {
		t.Fatalf("identity files = %v", got.IdentityFiles)
	}

	glob := lookupSSHConfigIn(path, "prod-web1")
	if glob.User != "deploy" {
		t.Fatalf("glob user = %q", glob.User)
	}

	suffix := lookupSSHConfigIn(path, "db.internal")
	if suffix.User != "deploy" {
		t.Fatalf("suffix glob user = %q", suffix.User)
	}

	// An alias with an explicit port still matches its Host block.
	withPort := lookupSSHConfigIn(path, "bastion:22")
	if withPort.HostName != "bastion.example.com" {
		t.Fatalf("with port = %+v", withPort)
	}

	// Unknown host only picks up the wildcard block.
	unknown := lookupSSHConfigIn(path, "nothing-matches-this")
	if unknown.HostName != "" || unknown.User != "" || len(unknown.IdentityFiles) != 1 {
		t.Fatalf("unknown = %+v", unknown)
	}
}

func TestLookupSSHConfigNegationAndEquals(t *testing.T) {
	path := writeConfig(t, `
Host *.example.com !secret.example.com
    User general

Host secret.example.com
    User=hidden
    IdentityFile="~/.ssh/secret key"
`)
	if got := lookupSSHConfigIn(path, "app.example.com"); got.User != "general" {
		t.Fatalf("app = %+v", got)
	}
	got := lookupSSHConfigIn(path, "secret.example.com")
	if got.User != "hidden" {
		t.Fatalf("secret user = %q", got.User)
	}
	if len(got.IdentityFiles) != 1 || filepath.Base(got.IdentityFiles[0]) != "secret key" {
		t.Fatalf("secret identity = %v", got.IdentityFiles)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"prod-*", "prod-web", true},
		{"prod-*", "dev-web", false},
		{"*.internal", "db.internal", true},
		{"*.internal", "db.external", false},
		{"web?", "web1", true},
		{"web?", "web12", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
	}
	for _, c := range cases {
		if got := matchPattern(c.pattern, c.s); got != c.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestMissingConfigIsHarmless(t *testing.T) {
	got := lookupSSHConfigIn(filepath.Join(t.TempDir(), "nope"), "host")
	if got.HostName != "" || len(got.IdentityFiles) != 0 {
		t.Fatalf("got %+v", got)
	}
}
