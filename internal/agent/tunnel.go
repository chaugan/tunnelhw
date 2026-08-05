package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chaugan/tunnelhw/internal/mux"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
)

const (
	helloTimeout   = 15 * time.Second
	openHdrTimeout = 15 * time.Second
	wsReadLimit    = 2 << 20 // carrier frames for the mux; not a policy cap
	backoffMin     = time.Second
	backoffMax     = time.Minute
)

// Tunnel maintains the outbound connection to the relay and serves device
// opens arriving over it.
type Tunnel struct {
	Core        *Core
	URL         string
	AgentID     string
	Credential  string
	InsecureDev bool

	state    atomic.Value // string: disconnected|connecting|connected
	lastErr  atomic.Value // string
	lastPong atomic.Int64 // unix nanos of last pong (one session at a time)
	mu       sync.Mutex
	ctrl     *proto.Conn // nil when disconnected
	kill     func()      // closes current session (kill switch)
}

// Status reports the tunnel state for the UI.
func (t *Tunnel) Status() (state, lastErr string) {
	if v, ok := t.state.Load().(string); ok {
		state = v
	} else {
		state = "disconnected"
	}
	lastErr, _ = t.lastErr.Load().(string)
	return
}

// Disconnect severs the current tunnel session (kill switch); Run's loop
// will keep retrying unless its context is cancelled.
func (t *Tunnel) Disconnect() {
	t.mu.Lock()
	k := t.kill
	t.mu.Unlock()
	if k != nil {
		k()
	}
}

// Run dials and serves until ctx is cancelled, reconnecting with backoff.
func (t *Tunnel) Run(ctx context.Context) {
	if err := t.validateURL(); err != nil {
		t.fail(err)
		return
	}
	t.Core.OnChange(t.announce) // re-announce on every expose/claim change
	backoff := backoffMin
	for {
		t.state.Store("connecting")
		start := time.Now()
		err := t.session(ctx)
		t.state.Store("disconnected")
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			t.lastErr.Store(err.Error())
			t.Core.Activity().Add("tunnel", "relay connection lost: "+err.Error())
		}
		if time.Since(start) > time.Minute {
			backoff = backoffMin // it worked for a while; start fresh
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

func (t *Tunnel) validateURL() error {
	u, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("relay URL: %w", err)
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !t.InsecureDev {
			return errors.New("relay URL uses ws:// — TLS is required unless insecure_dev is set")
		}
	default:
		return fmt.Errorf("relay URL scheme %q: want wss://", u.Scheme)
	}
	return nil
}

func (t *Tunnel) fail(err error) {
	t.lastErr.Store(err.Error())
	t.state.Store("disconnected")
	t.Core.Activity().Add("error", err.Error())
}

// session runs one full connect → hello → serve cycle.
func (t *Tunnel) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dialCtx, t.URL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(wsReadLimit)
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	sess, err := mux.Client(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mux: %w", err)
	}
	defer sess.Close()

	// First stream we open is the control stream, by convention.
	raw, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("control stream: %w", err)
	}
	ctrl := proto.NewConn(raw)

	raw.SetDeadline(time.Now().Add(helloTimeout))
	if err := ctrl.Send(proto.TypeHello, uuid.NewString(), proto.Hello{
		AgentID:       t.AgentID,
		Credential:    t.Credential,
		ProtoVersions: []int{proto.Version},
		AgentVersion:  "tunnelhw-agent/0.1",
	}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	env, err := ctrl.Recv()
	if err != nil {
		return fmt.Errorf("hello reply: %w", err)
	}
	raw.SetDeadline(time.Time{})

	var heartbeat time.Duration
	switch env.Type {
	case proto.TypeHelloOK:
		ok, err := proto.Decode[proto.HelloOK](env)
		if err != nil {
			return err
		}
		heartbeat = time.Duration(ok.HeartbeatSec) * time.Second
		if heartbeat <= 0 {
			heartbeat = 30 * time.Second
		}
	case proto.TypeHelloErr:
		he, _ := proto.Decode[proto.HelloErr](env)
		return fmt.Errorf("relay rejected hello: %s", he.Reason)
	default:
		return fmt.Errorf("unexpected reply %q to hello", env.Type)
	}

	t.mu.Lock()
	t.ctrl = ctrl
	t.kill = func() { sess.Close() }
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.ctrl = nil
		t.kill = nil
		t.mu.Unlock()
	}()

	t.state.Store("connected")
	t.lastErr.Store("")
	t.Core.Activity().Add("tunnel", "connected to relay")
	t.announce()

	errCh := make(chan error, 3)
	go func() { errCh <- t.controlLoop(ctrl) }()
	go func() { errCh <- t.acceptLoop(sess) }()
	go func() { errCh <- t.heartbeatLoop(ctx, ctrl, sess, heartbeat) }()

	select {
	case err := <-errCh:
		sess.Close()
		t.Core.CloseAll("tunnel closed")
		return err
	case <-ctx.Done():
		sess.Close()
		t.Core.CloseAll("agent shutting down")
		return ctx.Err()
	}
}

// announce sends the current exposed-device set if connected.
func (t *Tunnel) announce() {
	t.mu.Lock()
	ctrl := t.ctrl
	t.mu.Unlock()
	if ctrl == nil {
		return
	}
	devs := t.Core.ExposedDevices()
	if devs == nil {
		devs = []proto.Device{}
	}
	ctrl.Send(proto.TypeAnnounce, "", proto.Announce{Devices: devs})
}

func (t *Tunnel) heartbeatLoop(ctx context.Context, ctrl *proto.Conn, sess *yamux.Session, interval time.Duration) error {
	t.lastPong.Store(time.Now().UnixNano())
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sess.CloseChan():
			return errors.New("session closed")
		case <-tick.C:
			if err := ctrl.Send(proto.TypePing, "", nil); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
			if time.Since(time.Unix(0, t.lastPong.Load())) > 2*interval+5*time.Second {
				return errors.New("heartbeat: no pong from relay")
			}
		}
	}
}

func (t *Tunnel) controlLoop(ctrl *proto.Conn) error {
	for {
		env, err := ctrl.Recv()
		if err != nil {
			return fmt.Errorf("control: %w", err)
		}
		switch env.Type {
		case proto.TypePing:
			ctrl.Send(proto.TypePong, env.Corr, nil)
		case proto.TypePong:
			t.lastPong.Store(time.Now().UnixNano())
		case proto.TypeSetParams:
			sp, err := proto.Decode[proto.SetParams](env)
			res := proto.Result{OK: false, Reason: "bad payload"}
			if err == nil {
				res = t.Core.SetParams(sp.SessionID, sp.Baud, sp.DTR, sp.RTS)
			}
			ctrl.Send(proto.TypeSetParamsResult, env.Corr, res)
		case proto.TypeDrain:
			d, err := proto.Decode[proto.Drain](env)
			res := proto.Result{OK: false, Reason: "bad payload"}
			if err == nil {
				res = t.Core.Drain(d.SessionID)
			}
			ctrl.Send(proto.TypeDrainResult, env.Corr, res)
		case proto.TypeSessionClosed:
			sc, err := proto.Decode[proto.SessionClosed](env)
			if err == nil {
				t.Core.CloseSession(sc.SessionID, "closed by relay")
			}
		default:
			// Unknown types are ignored for forward compatibility.
		}
	}
}

func (t *Tunnel) acceptLoop(sess *yamux.Session) error {
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go t.serveStream(stream)
	}
}

// serveStream handles one device-open data stream from the relay.
func (t *Tunnel) serveStream(stream *yamux.Stream) {
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(openHdrTimeout))
	br := proto.NewConn(stream) // reuse framing; header is one frame
	env, err := headerAsOpen(br)
	if err != nil {
		return
	}
	sess, resp := t.Core.OpenSession(env.DeviceID, env.Params)
	resp.Corr = env.Corr
	if err := proto.WriteHeaderFrame(stream, resp); err != nil {
		if sess != nil {
			t.Core.CloseSession(sess.ID, "handshake write failed")
		}
		return
	}
	if sess == nil {
		return
	}
	stream.SetDeadline(time.Time{})
	t.bridge(stream, br, sess)
}

// headerAsOpen reads the open-header frame off the stream.
func headerAsOpen(c *proto.Conn) (*proto.OpenRequest, error) {
	var req proto.OpenRequest
	if err := c.ReadInto(&req); err != nil {
		return nil, err
	}
	if req.DeviceID == "" {
		return nil, errors.New("open header missing device_id")
	}
	return &req, nil
}

// bridge pumps bytes both ways until either side ends, then closes the
// session. br carries any bytes already buffered past the header.
func (t *Tunnel) bridge(stream net.Conn, br *proto.Conn, sess *Session) {
	done := make(chan struct{}, 2)
	// consumer → device
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				if _, werr := sess.Port.Write(buf[:n]); werr != nil {
					break
				}
				sess.Count(0, n)
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	// device → consumer
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.Port.Read(buf)
			if n > 0 {
				if _, werr := stream.Write(buf[:n]); werr != nil {
					break
				}
				sess.Count(n, 0)
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	t.Core.CloseSession(sess.ID, "stream ended")
	stream.Close()
	<-done
}
