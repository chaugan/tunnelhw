// Package relay implements the server side of the tunnel: it accepts agent
// WebSocket connections, authenticates them, tracks their announced devices,
// and brokers device streams for the relay API.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/mux"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
)

const (
	helloTimeout = 15 * time.Second
	openTimeout  = 15 * time.Second
	ctrlRPCWait  = 10 * time.Second
	heartbeatSec = 30
	wsReadLimit  = 2 << 20
)

type pendingMap struct {
	mu sync.Mutex
	m  map[string]chan proto.Result
}

func (p *pendingMap) add(corr string) chan proto.Result {
	ch := make(chan proto.Result, 1)
	p.mu.Lock()
	p.m[corr] = ch
	p.mu.Unlock()
	return ch
}

func (p *pendingMap) resolve(corr string, r proto.Result) {
	p.mu.Lock()
	ch, ok := p.m[corr]
	delete(p.m, corr)
	p.mu.Unlock()
	if ok {
		ch <- r
	}
}

func (p *pendingMap) drop(corr string) {
	p.mu.Lock()
	delete(p.m, corr)
	p.mu.Unlock()
}

type agentConn struct {
	id string
	// epoch distinguishes one connection of an agent from the next. Counters
	// the agent keeps in memory, such as the per-device reset count, restart
	// when the agent process does, so a consumer holding a number from an
	// earlier connection must not compare it against a fresh one.
	epoch   uint64
	name    string
	ctrl    *proto.Conn
	sess    *yamux.Session
	pending pendingMap

	mu      sync.Mutex
	devices map[string]proto.Device // word-id -> device
}

// nextAgentEpoch hands out a fresh epoch per accepted agent connection.
var nextAgentEpoch atomic.Uint64

// DeviceView is a device qualified by its owning agent.
type DeviceView struct {
	AgentID   string       `json:"agent_id"`
	AgentName string       `json:"agent_name,omitempty"`
	Device    proto.Device `json:"device"`
}

// Hub tracks connected agents.
type Hub struct {
	auth *auth.Store

	mu     sync.Mutex
	agents map[string]*agentConn
}

// NewHub builds a hub over the credential store.
func NewHub(a *auth.Store) *Hub {
	return &Hub{auth: a, agents: map[string]*agentConn{}}
}

// HandleWS is the agent tunnel endpoint.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// This endpoint is dialed by the agent binary, never a browser, so the
		// browser Origin check does not apply. TLS is unaffected; agents are
		// authenticated by the hello credential exchange below.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	ws.SetReadLimit(wsReadLimit)
	conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	defer conn.Close()

	sess, err := mux.Server(conn)
	if err != nil {
		return
	}
	defer sess.Close()

	// First stream from the agent is the control stream, by convention.
	acceptCtx, cancel := context.WithTimeout(r.Context(), helloTimeout)
	raw, err := sess.AcceptStreamWithContext(acceptCtx)
	cancel()
	if err != nil {
		return
	}
	ctrl := proto.NewConn(raw)

	raw.SetDeadline(time.Now().Add(helloTimeout))
	env, err := ctrl.Recv()
	if err != nil || env.Type != proto.TypeHello {
		return
	}
	hello, err := proto.Decode[proto.Hello](env)
	if err != nil {
		return
	}
	if !h.auth.VerifyAgent(hello.AgentID, hello.Credential) {
		ctrl.Send(proto.TypeHelloErr, env.Corr, proto.HelloErr{Reason: "authentication failed"})
		return
	}
	ver, err := proto.Negotiate(hello.ProtoVersions)
	if err != nil {
		ctrl.Send(proto.TypeHelloErr, env.Corr, proto.HelloErr{Reason: err.Error()})
		return
	}
	if err := ctrl.Send(proto.TypeHelloOK, env.Corr, proto.HelloOK{ProtoVersion: ver, HeartbeatSec: heartbeatSec}); err != nil {
		return
	}
	raw.SetDeadline(time.Time{})

	name := ""
	if rec, ok := h.auth.Agents()[hello.AgentID]; ok {
		name = rec.Name
	}
	ac := &agentConn{
		id: hello.AgentID, epoch: nextAgentEpoch.Add(1), name: name, ctrl: ctrl, sess: sess,
		pending: pendingMap{m: map[string]chan proto.Result{}},
		devices: map[string]proto.Device{},
	}

	h.mu.Lock()
	if prev, ok := h.agents[hello.AgentID]; ok {
		prev.sess.Close() // one live connection per agent; newest wins
	}
	h.agents[hello.AgentID] = ac
	h.mu.Unlock()
	log.Printf("agent %s connected", hello.AgentID)

	defer func() {
		h.mu.Lock()
		if h.agents[hello.AgentID] == ac {
			delete(h.agents, hello.AgentID)
		}
		h.mu.Unlock()
		log.Printf("agent %s disconnected", hello.AgentID)
	}()

	h.controlLoop(ac) // blocks for the life of the connection
}

func (h *Hub) controlLoop(ac *agentConn) {
	for {
		env, err := ac.ctrl.Recv()
		if err != nil {
			return
		}
		switch env.Type {
		case proto.TypeAnnounce:
			ann, err := proto.Decode[proto.Announce](env)
			if err != nil {
				continue
			}
			devs := map[string]proto.Device{}
			for _, d := range ann.Devices {
				devs[d.ID] = d
			}
			ac.mu.Lock()
			ac.devices = devs
			ac.mu.Unlock()
		case proto.TypePing:
			ac.ctrl.Send(proto.TypePong, env.Corr, nil)
		case proto.TypePong:
			// yamux keepalive covers liveness; nothing to do.
		case proto.TypeSetParamsResult, proto.TypeDrainResult:
			res, err := proto.Decode[proto.Result](env)
			if err != nil {
				res = proto.Result{OK: false, Reason: "bad result payload"}
			}
			ac.pending.resolve(env.Corr, res)
		default:
			// Forward compatibility: ignore unknown types.
		}
	}
}

// Devices lists all announced devices, agent-qualified. If agentFilter is
// non-empty, only those agents are included (API token scoping).
func (h *Hub) Devices(agentFilter []string) []DeviceView {
	h.mu.Lock()
	agents := make([]*agentConn, 0, len(h.agents))
	for _, ac := range h.agents {
		agents = append(agents, ac)
	}
	h.mu.Unlock()

	var out []DeviceView
	for _, ac := range agents {
		if len(agentFilter) > 0 && !containsStr(agentFilter, ac.id) {
			continue
		}
		ac.mu.Lock()
		for _, d := range ac.devices {
			out = append(out, DeviceView{AgentID: ac.id, AgentName: ac.name, Device: d})
		}
		ac.mu.Unlock()
	}
	return out
}

// ResetsFor reports how many times the device with the given UUID has vanished
// and returned, as last announced by its agent, along with the epoch of the
// connection that reported it. The last result is false when the agent or the
// device is no longer connected, which callers treat as "no news" rather than
// as a reset.
//
// Devices are matched by UUID rather than by word-ID because a word-ID can be
// regenerated while a session is open, and a caller holding the old name would
// otherwise silently stop hearing about that device.
func (h *Hub) ResetsFor(agentID, deviceUUID string) (int, uint64, bool) {
	h.mu.Lock()
	ac := h.agents[agentID]
	h.mu.Unlock()
	if ac == nil {
		return 0, 0, false
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	for _, d := range ac.devices {
		if d.UUID == deviceUUID {
			return d.Meta.Resets, ac.epoch, true
		}
	}
	return 0, 0, false
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ErrAmbiguous is returned when a word-ID exists on several agents and the
// caller did not qualify it.
var ErrAmbiguous = errors.New("device id is ambiguous across agents; qualify as <agent_id>/<device-id>")

// resolve finds the agent owning deviceID. Accepts "word-id" or the
// qualified "agent_id/word-id" form.
func (h *Hub) resolve(deviceID string, agentFilter []string) (*agentConn, string, error) {
	wantAgent := ""
	if i := strings.IndexByte(deviceID, '/'); i >= 0 {
		wantAgent, deviceID = deviceID[:i], deviceID[i+1:]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var found *agentConn
	for _, ac := range h.agents {
		if wantAgent != "" && ac.id != wantAgent {
			continue
		}
		if len(agentFilter) > 0 && !containsStr(agentFilter, ac.id) {
			continue
		}
		ac.mu.Lock()
		_, has := ac.devices[deviceID]
		ac.mu.Unlock()
		if !has {
			continue
		}
		if found != nil {
			return nil, "", ErrAmbiguous
		}
		found = ac
	}
	if found == nil {
		return nil, "", fmt.Errorf("unknown device %q (is its agent connected and the device exposed?)", deviceID)
	}
	return found, deviceID, nil
}

// OpenedStream is a live device stream plus its identifiers.
type OpenedStream struct {
	AgentID string
	// DeviceID is the word-ID the session was opened with, for display.
	DeviceID string
	// DeviceUUID identifies the device even if its word-ID is regenerated.
	DeviceUUID string
	AgentEpoch uint64
	SessionID  string
	Conn       *proto.Conn // read side (serves buffered bytes); use Write on Raw for writes
	Raw        *yamux.Stream
}

// Open opens a device session on the owning agent and returns the live
// stream after the header exchange.
func (h *Hub) Open(deviceID string, params proto.OpenParams, agentFilter []string) (*OpenedStream, error) {
	ac, wordID, err := h.resolve(deviceID, agentFilter)
	if err != nil {
		return nil, err
	}
	ac.mu.Lock()
	deviceUUID := ac.devices[wordID].UUID
	ac.mu.Unlock()
	stream, err := ac.sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("agent stream: %w", err)
	}
	stream.SetDeadline(time.Now().Add(openTimeout))
	corr := uuid.NewString()
	if err := proto.WriteHeaderFrame(stream, proto.OpenRequest{Corr: corr, DeviceID: wordID, Params: params}); err != nil {
		stream.Close()
		return nil, err
	}
	pc := proto.NewConn(stream)
	var resp proto.OpenResponse
	if err := pc.ReadInto(&resp); err != nil {
		stream.Close()
		return nil, fmt.Errorf("open response: %w", err)
	}
	if !resp.OK {
		stream.Close()
		if resp.Busy {
			return nil, fmt.Errorf("device %s is busy (session %s)", wordID, resp.ClaimedBy)
		}
		return nil, errors.New(resp.Reason)
	}
	stream.SetDeadline(time.Time{})
	return &OpenedStream{
		AgentID: ac.id, DeviceID: wordID, DeviceUUID: deviceUUID,
		AgentEpoch: ac.epoch, SessionID: resp.SessionID, Conn: pc, Raw: stream,
	}, nil
}

// rpc sends a correlated control request to an agent and awaits the result.
func (h *Hub) rpc(agentID, typ string, payload any) proto.Result {
	h.mu.Lock()
	ac, ok := h.agents[agentID]
	h.mu.Unlock()
	if !ok {
		return proto.Result{OK: false, Reason: "agent not connected"}
	}
	corr := uuid.NewString()
	ch := ac.pending.add(corr)
	if err := ac.ctrl.Send(typ, corr, payload); err != nil {
		ac.pending.drop(corr)
		return proto.Result{OK: false, Reason: err.Error()}
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(ctrlRPCWait):
		ac.pending.drop(corr)
		return proto.Result{OK: false, Reason: "agent did not respond in time"}
	}
}

// SetParams forwards a line-parameter change to the owning agent.
func (h *Hub) SetParams(agentID, sessionID string, baud *int, dtr, rts *bool) proto.Result {
	return h.rpc(agentID, proto.TypeSetParams, proto.SetParams{SessionID: sessionID, Baud: baud, DTR: dtr, RTS: rts})
}

// Drain forwards a drain request.
func (h *Hub) Drain(agentID, sessionID string) proto.Result {
	return h.rpc(agentID, proto.TypeDrain, proto.Drain{SessionID: sessionID})
}

// NotifyClosed tells the agent a session was closed relay-side.
func (h *Hub) NotifyClosed(agentID, sessionID string) {
	h.mu.Lock()
	ac, ok := h.agents[agentID]
	h.mu.Unlock()
	if ok {
		ac.ctrl.Send(proto.TypeSessionClosed, "", proto.SessionClosed{SessionID: sessionID, Reason: "closed by consumer"})
	}
}
