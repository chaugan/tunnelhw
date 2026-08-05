// Package auth is the relay's credential store. Two separate principals,
// never conflated (design review): agents (pairing tokens → per-agent
// credentials) and API/MCP consumers (bearer tokens with scopes).
//
// Only SHA-256 hashes of secrets are stored; plaintext is shown exactly once
// at mint time. State persists as an atomic JSON file.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PairingTokenTTL bounds how long a minted pairing token can be used.
const PairingTokenTTL = 5 * time.Minute

type pairingToken struct {
	Hash      string    `json:"hash"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
}

// AgentRecord is a paired agent.
type AgentRecord struct {
	CredentialHash string    `json:"credential_hash"`
	Name           string    `json:"name,omitempty"`
	PairedAt       time.Time `json:"paired_at"`
	Revoked        bool      `json:"revoked"`
}

// APIToken is a consumer (LLM-host / internal API) bearer token. Scopes are
// recorded from day one; v1 enforcement is coarse (any valid token may use
// the API; read_only is honored).
type APIToken struct {
	Hash     string    `json:"hash"`
	Name     string    `json:"name,omitempty"`
	ReadOnly bool      `json:"read_only"`
	Agents   []string  `json:"agents,omitempty"` // empty = all agents
	MintedAt time.Time `json:"minted_at"`
	Revoked  bool      `json:"revoked"`
}

type state struct {
	PairingTokens []pairingToken         `json:"pairing_tokens"`
	Agents        map[string]AgentRecord `json:"agents"`
	APITokens     []APIToken             `json:"api_tokens"`
}

// Store is the persistent credential store.
//
// The file is shared between a long-running relay and the short-lived admin
// commands that mint and revoke credentials, so every operation re-reads it
// when it has changed on disk. Without that, a token minted by the CLI is
// invisible to the running relay until restart, and the next write from
// either process silently discards the other's changes.
type Store struct {
	mu      sync.Mutex
	path    string
	st      state
	modTime time.Time
	size    int64
}

// Open loads (or initializes) the store at dir/auth.json.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "auth.json"), st: state{Agents: map[string]AgentRecord{}}}
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.st); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", s.path, err)
	}
	if s.st.Agents == nil {
		s.st.Agents = map[string]AgentRecord{}
	}
	s.stampLocked()
	return s, nil
}

// stampLocked records the file's identity so reloadLocked can detect
// out-of-process writes.
func (s *Store) stampLocked() {
	if fi, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = fi.ModTime(), fi.Size()
	}
}

// reloadLocked re-reads the file if another process has written it.
func (s *Store) reloadLocked() {
	fi, err := os.Stat(s.path)
	if err != nil {
		return // absent: nothing on disk to be newer than memory
	}
	if fi.ModTime().Equal(s.modTime) && fi.Size() == s.size {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var fresh state
	if err := json.Unmarshal(raw, &fresh); err != nil {
		return // keep the good in-memory copy rather than trust a torn read
	}
	if fresh.Agents == nil {
		fresh.Agents = map[string]AgentRecord{}
	}
	s.st = fresh
	s.modTime, s.size = fi.ModTime(), fi.Size()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.stampLocked()
	return nil
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashOf(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func hashEqual(hash, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(hash), []byte(hashOf(secret))) == 1
}

// MintPairingToken creates a single-use, short-lived pairing token and
// returns its plaintext — the only time it exists outside a hash.
func (s *Store) MintPairingToken() (string, time.Time, error) {
	tok, err := newSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(PairingTokenTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	// Prune expired/used while we're here.
	kept := s.st.PairingTokens[:0]
	for _, p := range s.st.PairingTokens {
		if !p.Used && time.Now().Before(p.ExpiresAt) {
			kept = append(kept, p)
		}
	}
	s.st.PairingTokens = append(kept, pairingToken{Hash: hashOf(tok), ExpiresAt: exp})
	return tok, exp, s.saveLocked()
}

// ErrBadPairing is returned for invalid, expired, or reused pairing tokens —
// indistinguishably, on purpose.
var ErrBadPairing = errors.New("invalid or expired pairing token")

// ExchangePairing consumes a pairing token and mints the agent identity.
func (s *Store) ExchangePairing(token, name string) (agentID, credential string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	for i := range s.st.PairingTokens {
		p := &s.st.PairingTokens[i]
		if p.Used || time.Now().After(p.ExpiresAt) || !hashEqual(p.Hash, token) {
			continue
		}
		p.Used = true
		credential, err = newSecret()
		if err != nil {
			return "", "", err
		}
		agentID = uuid.NewString()
		s.st.Agents[agentID] = AgentRecord{CredentialHash: hashOf(credential), Name: name, PairedAt: time.Now()}
		return agentID, credential, s.saveLocked()
	}
	return "", "", ErrBadPairing
}

// VerifyAgent checks an agent credential.
func (s *Store) VerifyAgent(agentID, credential string) bool {
	s.mu.Lock()
	s.reloadLocked()
	rec, ok := s.st.Agents[agentID]
	s.mu.Unlock()
	return ok && !rec.Revoked && hashEqual(rec.CredentialHash, credential)
}

// RevokeAgent invalidates an agent's credential.
func (s *Store) RevokeAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	rec, ok := s.st.Agents[agentID]
	if !ok {
		return fmt.Errorf("auth: unknown agent %s", agentID)
	}
	rec.Revoked = true
	s.st.Agents[agentID] = rec
	return s.saveLocked()
}

// Agents lists paired agents (for the admin CLI).
func (s *Store) Agents() map[string]AgentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	out := make(map[string]AgentRecord, len(s.st.Agents))
	for k, v := range s.st.Agents {
		out[k] = v
	}
	return out
}

// MintAPIToken creates a consumer bearer token, returning its plaintext once.
func (s *Store) MintAPIToken(name string, readOnly bool, agents []string) (string, error) {
	tok, err := newSecret()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	s.st.APITokens = append(s.st.APITokens, APIToken{
		Hash: hashOf(tok), Name: name, ReadOnly: readOnly, Agents: agents, MintedAt: time.Now(),
	})
	return tok, s.saveLocked()
}

// VerifyAPIToken checks a bearer token and returns its scope record.
func (s *Store) VerifyAPIToken(token string) (*APIToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	for i := range s.st.APITokens {
		t := &s.st.APITokens[i]
		if !t.Revoked && hashEqual(t.Hash, token) {
			cp := *t
			return &cp, true
		}
	}
	return nil, false
}
