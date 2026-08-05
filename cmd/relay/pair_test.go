package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/relay"
	"github.com/chaugan/tunnelhw/internal/relayapi"
)

// newTestServer wires the real mux over a fresh store, returning both.
func newTestServer(t *testing.T, limiter *pairLimiter) (*httptest.Server, *auth.Store) {
	t.Helper()
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)
	api := &relayapi.API{Broker: broker, Auth: store}
	srv := httptest.NewServer(newServeMux(hub, api, store, limiter, true))
	t.Cleanup(srv.Close)
	return srv, store
}

func postPair(t *testing.T, url string, req pairRequest) (int, pairResponse) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url+"/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out pairResponse
	json.NewDecoder(resp.Body).Decode(&out) // error responses simply leave it zero
	return resp.StatusCode, out
}

func TestPairExchange(t *testing.T) {
	srv, store := newTestServer(t, newPairLimiter(pairLimit, pairWindow))

	tok, _, err := store.MintPairingToken()
	if err != nil {
		t.Fatal(err)
	}

	code, got := postPair(t, srv.URL, pairRequest{Token: tok, Name: "bench-box"})
	if code != http.StatusOK {
		t.Fatalf("pair with valid token: got %d, want 200", code)
	}
	if got.AgentID == "" || got.Credential == "" {
		t.Fatalf("pair response missing fields: %+v", got)
	}
	if !store.VerifyAgent(got.AgentID, got.Credential) {
		t.Fatal("minted credential does not verify against the store")
	}
	rec, ok := store.Agents()[got.AgentID]
	if !ok || rec.Name != "bench-box" {
		t.Fatalf("agent record not persisted with name: %+v ok=%v", rec, ok)
	}

	// Single use: replaying the consumed token must fail.
	if code, _ := postPair(t, srv.URL, pairRequest{Token: tok, Name: "again"}); code != http.StatusForbidden {
		t.Fatalf("replayed token: got %d, want 403", code)
	}
}

func TestPairBadToken(t *testing.T) {
	srv, store := newTestServer(t, newPairLimiter(pairLimit, pairWindow))

	code, _ := postPair(t, srv.URL, pairRequest{Token: "not-a-real-token", Name: "x"})
	if code != http.StatusForbidden {
		t.Fatalf("bad token: got %d, want 403", code)
	}
	if n := len(store.Agents()); n != 0 {
		t.Fatalf("bad token minted %d agents, want 0", n)
	}
}

func TestPairRateLimit(t *testing.T) {
	srv, store := newTestServer(t, newPairLimiter(pairLimit, pairWindow))

	for i := 0; i < pairLimit; i++ {
		if code, _ := postPair(t, srv.URL, pairRequest{Token: "wrong"}); code != http.StatusForbidden {
			t.Fatalf("attempt %d: got %d, want 403", i+1, code)
		}
	}
	if code, _ := postPair(t, srv.URL, pairRequest{Token: "wrong"}); code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: got %d, want 429", pairLimit+1, code)
	}

	// Even a valid token is limited: the limiter runs before the exchange.
	tok, _, err := store.MintPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := postPair(t, srv.URL, pairRequest{Token: tok}); code != http.StatusTooManyRequests {
		t.Fatalf("valid token during limit: got %d, want 429", code)
	}
}

func TestPairLimiterWindowReset(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newPairLimiter(2, time.Minute)
	l.now = func() time.Time { return now }

	if !l.allow() || !l.allow() {
		t.Fatal("first two attempts should be allowed")
	}
	if l.allow() {
		t.Fatal("third attempt in window should be denied")
	}
	now = now.Add(time.Minute)
	if !l.allow() {
		t.Fatal("attempt in the next window should be allowed")
	}
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, newPairLimiter(pairLimit, pairWindow))
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", resp.StatusCode)
	}
}
