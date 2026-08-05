package auth

import "testing"

func TestPairingLifecycle(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.MintPairingToken()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.ExchangePairing("wrong-token", "x"); err != ErrBadPairing {
		t.Fatalf("wrong token: err = %v", err)
	}
	id, cred, err := s.ExchangePairing(tok, "my-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !s.VerifyAgent(id, cred) {
		t.Fatal("fresh credential must verify")
	}
	// Single use: second exchange fails.
	if _, _, err := s.ExchangePairing(tok, "again"); err != ErrBadPairing {
		t.Fatalf("reuse: err = %v", err)
	}
	// Revocation sticks.
	if err := s.RevokeAgent(id); err != nil {
		t.Fatal(err)
	}
	if s.VerifyAgent(id, cred) {
		t.Fatal("revoked credential must not verify")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir)
	tok, _, _ := s1.MintPairingToken()
	id, cred, err := s1.ExchangePairing(tok, "")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.VerifyAgent(id, cred) {
		t.Fatal("credential must survive reload")
	}
}

func TestAPITokens(t *testing.T) {
	s, _ := Open(t.TempDir())
	tok, err := s.MintAPIToken("llm-host", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := s.VerifyAPIToken(tok)
	if !ok || !rec.ReadOnly || rec.Name != "llm-host" {
		t.Fatalf("verify = %+v, %v", rec, ok)
	}
	if _, ok := s.VerifyAPIToken("nope"); ok {
		t.Fatal("bad token must not verify")
	}
}

// A running relay and an admin CLI are two processes over one file. Tokens
// minted by one must be honored by the other, and neither may clobber the
// other's writes.
func TestCrossProcessVisibility(t *testing.T) {
	dir := t.TempDir()
	relay, err := Open(dir) // long-running
	if err != nil {
		t.Fatal(err)
	}
	cli, err := Open(dir) // short-lived admin command
	if err != nil {
		t.Fatal(err)
	}

	apiTok, err := cli.MintAPIToken("llm-host", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := relay.VerifyAPIToken(apiTok); !ok {
		t.Fatal("relay must honor a token minted by the admin CLI without a restart")
	}

	// The relay pairs an agent (writing the file), then the CLI mints again.
	pairTok, _, err := cli.MintPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	agentID, cred, err := relay.ExchangePairing(pairTok, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cli.MintAPIToken("second", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Neither write may have discarded the other.
	if !relay.VerifyAgent(agentID, cred) {
		t.Fatal("agent pairing lost after a later CLI write")
	}
	if _, ok := relay.VerifyAPIToken(second); !ok {
		t.Fatal("second CLI token not visible to the relay")
	}
	if _, ok := relay.VerifyAPIToken(apiTok); !ok {
		t.Fatal("first CLI token lost")
	}
}
