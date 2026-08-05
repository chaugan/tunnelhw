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

// Addr returns the host:port to dial.
func (c Config) Addr() string {
	if c.Host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(c.Host); err == nil {
		return c.Host
	}
	return net.JoinHostPort(c.Host, DefaultPort)
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
	return fmt.Sprintf("unknown SSH host key for %s (fingerprint %s) — verify it, then accept it to continue", e.Host, e.Fingerprint)
}

// ChangedHostKeyError is returned when the server presents a different key
// than the one recorded. This is never auto-accepted.
type ChangedHostKeyError struct {
	Host        string
	Fingerprint string
}

func (e *ChangedHostKeyError) Error() string {
	return fmt.Sprintf("SSH host key for %s CHANGED (now %s) — refusing to connect; if this is expected, remove the old entry from known_hosts", e.Host, e.Fingerprint)
}

// Client is a live SSH connection used as a dialer.
type Client struct {
	cfg Config

	mu     sync.Mutex
	client *ssh.Client
}

// Dial establishes the SSH connection.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if !cfg.Valid() {
		return nil, errors.New("sshtun: ssh host and user are required")
	}
	auths, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
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
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: hostKey,
		Timeout:         15 * time.Second,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("sshtun: ssh handshake with %s: %w", addr, err)
	}
	conn.SetDeadline(time.Time{})
	return &Client{cfg: cfg, client: ssh.NewClient(sc, chans, reqs)}, nil
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

// authMethods assembles auth in preference order: explicit key, the usual
// ~/.ssh keys, then password.
func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	add := func(path string, required bool) error {
		raw, err := os.ReadFile(path)
		if err != nil {
			if required {
				return fmt.Errorf("sshtun: read key %s: %w", path, err)
			}
			return nil
		}
		var signer ssh.Signer
		if cfg.KeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(cfg.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(raw)
		}
		if err != nil {
			if required {
				return fmt.Errorf("sshtun: parse key %s: %w", path, err)
			}
			return nil
		}
		methods = append(methods, ssh.PublicKeys(signer))
		return nil
	}

	if cfg.KeyPath != "" {
		if err := add(cfg.KeyPath, true); err != nil {
			return nil, err
		}
	} else if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			add(filepath.Join(home, ".ssh", name), false)
		}
	}
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
		methods = append(methods, ssh.KeyboardInteractive(
			func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = cfg.Password
				}
				return answers, nil
			}))
	}
	if len(methods) == 0 {
		return nil, errors.New("sshtun: no usable SSH credentials — set a key file or a password")
	}
	return methods, nil
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
			// A key is on file and it is not this one. Never auto-accept.
			return &ChangedHostKeyError{Host: hostname, Fingerprint: ssh.FingerprintSHA256(key)}
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
