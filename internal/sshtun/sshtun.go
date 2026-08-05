// Package sshtun dials the relay through an SSH server instead of over the
// open network.
//
// This is the deployment that needs no public address anywhere: the relay
// runs on the same machine as the LLM host, bound to loopback, and the agent
// reaches it by opening an SSH connection outbound and asking the SSH server
// to connect onward to its own 127.0.0.1:<relay port>. SSH provides the
// encryption and the server authentication, so the tunnel protocol rides over
// it unchanged and plaintext ws:// inside the SSH channel is correct rather
// than a downgrade.
//
// Host keys are verified against known_hosts. An unfamiliar host is a hard
// error carrying the fingerprint (so a UI can show it and ask), never a
// silent accept; a *changed* host key always fails.
package sshtun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DefaultPort is the SSH port assumed when the host carries none.
const DefaultPort = "22"

// Config describes how to reach the SSH server that fronts the relay.
type Config struct {
	// Host is the SSH server, "host" or "host:port" (default port 22).
	Host string `json:"host"`
	// User is the SSH login name.
	User string `json:"user"`
	// KeyPath is a private key file. Empty means: try the usual
	// ~/.ssh/id_* keys, then Password.
	KeyPath string `json:"key_path,omitempty"`
	// KeyPassphrase decrypts KeyPath when it is encrypted.
	KeyPassphrase string `json:"key_passphrase,omitempty"`
	// Password is used for password auth when no key works.
	Password string `json:"password,omitempty"`
	// KnownHostsPath overrides ~/.ssh/known_hosts.
	KnownHostsPath string `json:"known_hosts_path,omitempty"`
	// AcceptNewHostKey records an unfamiliar host key instead of failing.
	// It never overrides a *changed* key. Set it only after a human has
	// seen and approved the fingerprint.
	AcceptNewHostKey bool `json:"accept_new_host_key,omitempty"`
}

// Addr returns the host:port to dial, after applying any ~/.ssh/config entry
// for the host (so a short alias resolves exactly as it does in a terminal).
func (c Config) Addr() string {
	if c.Host == "" {
		return ""
	}
	return c.resolve().addr
}

// resolved is a Config with ~/.ssh/config applied.
type resolved struct {
	addr          string
	user          string
	identityFiles []string
	viaConfig     bool
}

func (c Config) resolve() resolved {
	host, port, hadPort := splitHostPort(c.Host)
	out := resolved{user: c.User}

	sc := lookupSSHConfig(host)
	if sc.HostName != "" || sc.User != "" || sc.Port != "" || len(sc.IdentityFiles) > 0 {
		out.viaConfig = true
	}
	if sc.HostName != "" {
		host = sc.HostName
	}
	if !hadPort {
		port = sc.Port
	}
	if port == "" {
		port = DefaultPort
	}
	if out.user == "" {
		out.user = sc.User
	}
	out.identityFiles = sc.IdentityFiles
	out.addr = net.JoinHostPort(host, port)
	return out
}

// Valid reports whether the config is complete enough to dial.
func (c Config) Valid() bool { return c.Host != "" && c.User != "" }

// UnknownHostKeyError is returned when the SSH server is not in known_hosts.
// It carries the fingerprint so a UI can show it for approval; approving means
// re-dialing with AcceptNewHostKey set.
type UnknownHostKeyError struct {
	Host        string
	Fingerprint string
}

func (e *UnknownHostKeyError) Error() string {
	return fmt.Sprintf("unknown SSH host key for %s (fingerprint %s): verify it, then accept it to continue", e.Host, e.Fingerprint)
}

// ChangedHostKeyError is returned when the server presents a different key
// than the one recorded. This is never auto-accepted.
type ChangedHostKeyError struct {
	Host        string
	Fingerprint string
}

func (e *ChangedHostKeyError) Error() string {
	return fmt.Sprintf("SSH host key for %s CHANGED (now %s). Refusing to connect; if this is expected, remove the old entry from known_hosts", e.Host, e.Fingerprint)
}

// Client is a live SSH connection used as a dialer.
type Client struct {
	cfg Config

	mu     sync.Mutex
	client *ssh.Client
}

// hostKeyMismatch means known_hosts has key(s) for this host but not the one
// presented. Usually that is an algorithm mismatch rather than a real change:
// a server offers several host keys (ed25519, ecdsa, rsa) and known_hosts
// typically records only the one OpenSSH negotiated. It carries the
// algorithms that ARE on file so the dial can be retried pinned to them,
// which is what OpenSSH does.
type hostKeyMismatch struct {
	host        string
	fingerprint string
	knownAlgos  []string
}

func (e *hostKeyMismatch) Error() string {
	return fmt.Sprintf("host key mismatch for %s (presented %s)", e.host, e.fingerprint)
}

// Dial establishes the SSH connection.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if !cfg.Valid() {
		return nil, errors.New("sshtun: ssh host and user are required")
	}
	c, err := dial(ctx, cfg, nil)
	if err == nil {
		return c, nil
	}
	// A mismatch on the first attempt is not yet evidence of a changed key:
	// we may simply have negotiated an algorithm known_hosts has no entry
	// for. Retry pinned to the algorithms actually on file (OpenSSH's
	// behaviour) and only then believe it.
	var mismatch *hostKeyMismatch
	if !errors.As(err, &mismatch) || len(mismatch.knownAlgos) == 0 {
		return nil, err
	}
	c, retryErr := dial(ctx, cfg, mismatch.knownAlgos)
	if retryErr == nil {
		return c, nil
	}
	var confirmed *hostKeyMismatch
	if errors.As(retryErr, &confirmed) {
		return nil, &ChangedHostKeyError{Host: confirmed.host, Fingerprint: confirmed.fingerprint}
	}
	return nil, retryErr
}

// dial performs one handshake. hostKeyAlgos, when non-empty, restricts the
// host key algorithms offered.
func dial(ctx context.Context, cfg Config, hostKeyAlgos []string) (*Client, error) {
	plan, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	defer plan.close()
	hostKey, err := hostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}
	addr := cfg.Addr()

	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshtun: dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	user := cfg.resolve().user
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:              user,
		Auth:              plan.methods,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           15 * time.Second,
	})
	if err != nil {
		conn.Close()
		// Unwrap our own host-key errors so callers can inspect them rather
		// than a string-formatted handshake failure.
		var unknown *UnknownHostKeyError
		var mismatch *hostKeyMismatch
		if errors.As(err, &unknown) {
			return nil, unknown
		}
		if errors.As(err, &mismatch) {
			return nil, mismatch
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("sshtun: %s@%s rejected our credentials. Tried: %s. "+
				"If your key is not in this list, set its exact path in the key field "+
				"(run `ssh -v %s@%s true` to see which key your own ssh uses)",
				user, addr, plan.Summary(), user, addr)
		}
		return nil, fmt.Errorf("sshtun: ssh handshake with %s: %w", addr, err)
	}
	conn.SetDeadline(time.Time{})
	return &Client{cfg: cfg, client: ssh.NewClient(sc, chans, reqs)}, nil
}

// algosFor lists the host key algorithms recorded for a host. An "ssh-rsa"
// entry also implies the SHA-2 signature algorithms for the same key, which
// modern servers require now that SHA-1 is disabled.
func algosFor(known []knownhosts.KnownKey) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, k := range known {
		t := k.Key.Type()
		add(t)
		if t == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSASHA512)
		}
	}
	return out
}

// DialContext opens a connection *from the SSH server* to addr. Addresses are
// therefore resolved on the server: "127.0.0.1:8443" means the relay bound to
// loopback on the SSH host. Matches net.Dialer.DialContext so it can be
// dropped into an http.Transport.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c.mu.Lock()
	cl := c.client
	c.mu.Unlock()
	if cl == nil {
		return nil, errors.New("sshtun: connection closed")
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := cl.Dial(network, addr)
		ch <- result{conn, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("sshtun: forward to %s: %w", addr, r.err)
		}
		return r.conn, nil
	}
}

// Wait blocks until the SSH connection ends.
func (c *Client) Wait() error {
	c.mu.Lock()
	cl := c.client
	c.mu.Unlock()
	if cl == nil {
		return errors.New("sshtun: connection closed")
	}
	return cl.Wait()
}

// Close ends the SSH connection.
func (c *Client) Close() error {
	c.mu.Lock()
	cl := c.client
	c.client = nil
	c.mu.Unlock()
	if cl == nil {
		return nil
	}
	return cl.Close()
}

// authPlan is the assembled credential set plus a human-readable account of
// what was tried. "unable to authenticate" is useless on its own; the tried
// list is what lets a user see that their key was skipped because it needs a
// passphrase, or that the agent held no keys.
type authPlan struct {
	methods []ssh.AuthMethod
	tried   []string
	closers []func()
}

func (p *authPlan) close() {
	for _, c := range p.closers {
		c()
	}
}

// Summary describes the credentials attempted, for error messages.
func (p *authPlan) Summary() string {
	if len(p.tried) == 0 {
		return "no credentials available"
	}
	return strings.Join(p.tried, "; ")
}

// authMethods assembles auth in preference order: explicit key, the running
// ssh-agent, the usual ~/.ssh keys, then password.
func authMethods(cfg Config) (*authPlan, error) {
	p := &authPlan{}

	addKeyFile := func(path string, required bool) error {
		raw, err := os.ReadFile(path)
		if err != nil {
			if required {
				return fmt.Errorf("sshtun: read key %s: %w", path, err)
			}
			return nil // absent default key: not worth mentioning
		}
		var signer ssh.Signer
		if cfg.KeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(cfg.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(raw)
		}
		if err != nil {
			var needsPass *ssh.PassphraseMissingError
			if errors.As(err, &needsPass) {
				p.tried = append(p.tried, path+" (SKIPPED: encrypted, needs a passphrase)")
				if required {
					return fmt.Errorf("sshtun: key %s is passphrase-protected; supply the passphrase", path)
				}
				return nil
			}
			if required {
				return fmt.Errorf("sshtun: parse key %s: %w", path, err)
			}
			p.tried = append(p.tried, path+" (SKIPPED: unreadable as a private key)")
			return nil
		}
		p.tried = append(p.tried, fmt.Sprintf("%s (%s)", path, ssh.FingerprintSHA256(signer.PublicKey())))
		p.methods = append(p.methods, ssh.PublicKeys(signer))
		return nil
	}

	if cfg.KeyPath != "" {
		if err := addKeyFile(cfg.KeyPath, true); err != nil {
			return nil, err
		}
	} else {
		// IdentityFile entries from ~/.ssh/config: the key the user's own
		// ssh command would pick for this host.
		for _, id := range cfg.resolve().identityFiles {
			if err := addKeyFile(id, false); err != nil {
				return nil, err
			}
		}
	}

	// The ssh-agent is where a key lives when login is passwordless but no
	// usable key file is on disk (or the file is encrypted and unlocked once
	// via ssh-add). On Windows this is a named pipe, not SSH_AUTH_SOCK.
	if conn, err := dialAgent(); err == nil {
		ag := agent.NewClient(conn)
		signers, sErr := ag.Signers()
		if sErr == nil && len(signers) > 0 {
			p.methods = append(p.methods, ssh.PublicKeys(signers...))
			prints := make([]string, 0, len(signers))
			for _, s := range signers {
				prints = append(prints, ssh.FingerprintSHA256(s.PublicKey()))
			}
			p.tried = append(p.tried, fmt.Sprintf("ssh-agent at %s [%s]", agentAddr(), strings.Join(prints, ", ")))
			p.closers = append(p.closers, func() { conn.Close() })
		} else {
			conn.Close()
			p.tried = append(p.tried, fmt.Sprintf("ssh-agent at %s (no keys loaded)", agentAddr()))
		}
	}

	if cfg.KeyPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
				if err := addKeyFile(filepath.Join(home, ".ssh", name), false); err != nil {
					return nil, err
				}
			}
		}
	}

	if cfg.Password != "" {
		p.methods = append(p.methods, ssh.Password(cfg.Password))
		p.methods = append(p.methods, ssh.KeyboardInteractive(
			func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = cfg.Password
				}
				return answers, nil
			}))
		p.tried = append(p.tried, "password")
	}

	if len(p.methods) == 0 {
		return nil, fmt.Errorf("sshtun: no usable SSH credentials; set a key file or password. Looked at: %s", p.Summary())
	}
	return p, nil
}

// knownHostsPath resolves the known_hosts file, creating it if absent.
func knownHostsPath(cfg Config) (string, error) {
	path := cfg.KnownHostsPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	f.Close()
	return path, nil
}

// hostKeyCallback verifies against known_hosts: unknown hosts surface a
// fingerprint for human approval, changed keys always fail.
func hostKeyCallback(cfg Config) (ssh.HostKeyCallback, error) {
	path, err := knownHostsPath(cfg)
	if err != nil {
		return nil, fmt.Errorf("sshtun: known_hosts: %w", err)
	}
	verify, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("sshtun: parse known_hosts %s: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}
		var kerr *knownhosts.KeyError
		if !errors.As(err, &kerr) {
			return err
		}
		if len(kerr.Want) > 0 {
			// Keys are on file but not this one. Could be a genuine change or
			// just an algorithm we have no entry for. Dial retries pinned to
			// the recorded algorithms before concluding anything.
			return &hostKeyMismatch{
				host:        hostname,
				fingerprint: ssh.FingerprintSHA256(key),
				knownAlgos:  algosFor(kerr.Want),
			}
		}
		if !cfg.AcceptNewHostKey {
			return &UnknownHostKeyError{Host: hostname, Fingerprint: ssh.FingerprintSHA256(key)}
		}
		return appendKnownHost(path, hostname, remote, key)
	}, nil
}

// appendKnownHost records a newly approved host key.
func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	addrs := []string{knownhosts.Normalize(hostname)}
	if remote != nil {
		if ra := knownhosts.Normalize(remote.String()); ra != addrs[0] {
			addrs = append(addrs, ra)
		}
	}
	line := knownhosts.Line(addrs, key)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err = f.WriteString(line)
	return err
}
