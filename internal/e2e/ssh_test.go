package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chaugan/tunnelhw/internal/agent"
	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/relay"
	"github.com/chaugan/tunnelhw/internal/relayapi"
	"github.com/chaugan/tunnelhw/internal/serialdev"
	"github.com/chaugan/tunnelhw/internal/sshtun"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshServer is a minimal in-process sshd that serves direct-tcpip channels,
// exactly the forwarding TunnelHW relies on. It stands in for the sshd on the
// LLM machine.
type sshServer struct {
	addr string
	// signer is the ed25519 host key, the one OpenSSH prefers and therefore
	// the one that ends up in known_hosts. The server also offers ECDSA,
	// which Go's client prefers, so the two disagree exactly as a real
	// server does.
	signer      ssh.Signer
	ecdsaSigner ssh.Signer
	ln          net.Listener
}

func startSSHServer(t *testing.T, user, password string) *sshServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSigner, err := ssh.NewSignerFromKey(ecPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(pass) == password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)
	cfg.AddHostKey(ecSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sshServer{addr: ln.Addr().String(), signer: signer, ecdsaSigner: ecSigner, ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, cfg)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *sshServer) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			newCh.Reject(ssh.UnknownChannelType, "only direct-tcpip")
			continue
		}
		var payload struct {
			Host       string
			Port       uint32
			OriginHost string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
			newCh.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}
		target, err := net.Dial("tcp", net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))))
		if err != nil {
			newCh.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			target.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go func() {
			defer ch.Close()
			defer target.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(ch, target); done <- struct{}{} }()
			go func() { io.Copy(target, ch); done <- struct{}{} }()
			<-done
		}()
	}
}

// TestEndToEndOverSSH is the deployment that needs no public address: the
// relay is only reachable through the SSH host, and the agent tunnels to it
// over SSH. Proves host-key policy and the full device path together.
func TestEndToEndOverSSH(t *testing.T) {
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := relay.NewHub(store)
	broker := relayapi.NewBroker(hub)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.HandleWS)
	relaySrv := httptest.NewServer(mux)
	defer relaySrv.Close()

	sshd := startSSHServer(t, "tester", "hunter2")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")

	tok, _, err := store.MintPairingToken()
	if err != nil {
		t.Fatal(err)
	}
	agentID, cred, err := store.ExchangePairing(tok, "ssh-agent")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ports := []serialdev.PortInfo{{Path: "COM3", SerialNumber: "", Product: "Native COM port"}}
	core := agent.New(dir, cfg,
		func() ([]serialdev.PortInfo, error) { return ports, nil },
		func(path string, p proto.OpenParams) (serialdev.Port, error) { return newFakePort(), nil })
	if err := core.Rescan(); err != nil {
		t.Fatal(err)
	}
	devs := core.UIDevices()
	if len(devs) != 1 {
		t.Fatalf("UIDevices = %d, want 1", len(devs))
	}
	if err := core.SetExposed(devs[0].UUID, true); err != nil {
		t.Fatal(err)
	}

	sshCfg := &sshtun.Config{
		Host:           sshd.addr,
		User:           "tester",
		Password:       "hunter2",
		KnownHostsPath: knownHosts,
	}

	// An unfamiliar host key must be refused, with a fingerprint to show a human.
	if _, err := sshtun.Dial(context.Background(), *sshCfg); err == nil {
		t.Fatal("unknown host key must be refused")
	} else {
		var unknown *sshtun.UnknownHostKeyError
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want UnknownHostKeyError", err)
		}
		if !strings.HasPrefix(unknown.Fingerprint, "SHA256:") {
			t.Fatalf("fingerprint = %q", unknown.Fingerprint)
		}
	}

	// Approved: connect, and the key is recorded for next time.
	sshCfg.AcceptNewHostKey = true
	// The relay URL is resolved on the SSH host: plaintext ws:// inside the
	// SSH channel, and InsecureDev deliberately stays false.
	tun := &agent.Tunnel{
		Core:       core,
		URL:        "ws://" + relaySrv.Listener.Addr().String() + "/ws",
		AgentID:    agentID,
		Credential: cred,
		SSH:        sshCfg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tun.Run(ctx)

	waitFor(t, "tunnel connected over SSH", func() bool { s, _ := tun.Status(); return s == "connected" })
	waitFor(t, "device announced", func() bool { return len(hub.Devices(nil)) == 1 })

	if raw, err := os.ReadFile(knownHosts); err != nil || len(raw) == 0 {
		t.Fatalf("known_hosts not recorded: %v", err)
	}

	// Full device round trip through the SSH channel.
	sess, err := broker.Open(devs[0].ID, proto.OpenParams{Baud: 9600}, nil, "ssh-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Write([]byte("ping over ssh\n")); err != nil {
		t.Fatal(err)
	}
	res := sess.Read(5*time.Second, 1024, []byte("\n"))
	if got := string(res.Data); got != "ping over ssh\n" {
		t.Fatalf("read %q", got)
	}
	broker.Close(sess.ID)
}

// TestSSHChangedHostKeyRefused proves a swapped host key is never silently
// accepted, even with AcceptNewHostKey set.
func TestSSHChangedHostKeyRefused(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	first := startSSHServer(t, "u", "p")

	cfg := sshtun.Config{
		Host: first.addr, User: "u", Password: "p",
		KnownHostsPath: knownHosts, AcceptNewHostKey: true,
	}
	c, err := sshtun.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	c.Close()

	// Record the FIRST server's key against the SECOND server's address: the
	// second server will now present a key that contradicts the record,
	// the impersonation case known_hosts exists to catch.
	other := startSSHServer(t, "u", "p")
	line := knownhosts.Line([]string{knownhosts.Normalize(other.addr)}, first.signer.PublicKey())
	if err := os.WriteFile(knownHosts, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg.Host = other.addr
	_, err = sshtun.Dial(context.Background(), cfg)
	if err == nil {
		t.Fatal("changed host key must be refused even with AcceptNewHostKey")
	}
	var changed *sshtun.ChangedHostKeyError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want ChangedHostKeyError", err)
	}
}

// A server offers several host keys; OpenSSH records only the one it
// negotiated (ed25519), while Go's client prefers ECDSA. The client must
// recognise the host anyway; reporting "host key CHANGED" here is a false
// alarm that reads as an attack to the user.
func TestSSHKnownHostsAlgorithmMismatch(t *testing.T) {
	srv := startSSHServer(t, "u", "p")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")

	// Exactly what OpenSSH would have written: the ed25519 key only.
	line := knownhosts.Line([]string{knownhosts.Normalize(srv.addr)}, srv.signer.PublicKey())
	if err := os.WriteFile(knownHosts, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := sshtun.Config{
		Host: srv.addr, User: "u", Password: "p",
		KnownHostsPath: knownHosts,
		// Deliberately false: the host is already known, so no approval
		// should be needed and none may be silently granted.
		AcceptNewHostKey: false,
	}
	c, err := sshtun.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("known host with a different algorithm on file must connect, got: %v", err)
	}
	c.Close()
}
