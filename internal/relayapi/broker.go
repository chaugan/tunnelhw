// Package relayapi is the relay's versioned core API: session brokerage with
// LLM-safe read/write semantics. The MCP server and the HTTP API are both
// thin adapters over this package — it is the tested seam.
package relayapi

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/relay"
)

// Guardrails (design review: bounded reads, caps, no indefinite blocking).
const (
	MaxReadBytes    = 256 * 1024
	DefaultReadMax  = 4096
	MaxReadTimeout  = 60 * time.Second
	DefaultTimeout  = 2 * time.Second
	MaxWriteBytes   = 256 * 1024
	sessionBufCap   = 1 << 20 // per-session receive buffer; backpressure beyond
)

// Session is one live device session brokered by the relay. Owner is the
// identity of the credential that opened it (a token hash) — other
// credentials cannot see or touch the session (design review: a leaked
// read-only token must not be able to drain another principal's session).
type Session struct {
	ID       string    `json:"session_id"`
	AgentID  string    `json:"agent_id"`
	DeviceID string    `json:"device_id"`
	Opened   time.Time `json:"opened"`
	Owner    string    `json:"-"`

	stream *relay.OpenedStream

	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
	rdErr  error

	bytesIn, bytesOut uint64
}

// Broker owns live sessions.
type Broker struct {
	hub *relay.Hub

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewBroker builds a broker over the hub.
func NewBroker(hub *relay.Hub) *Broker {
	return &Broker{hub: hub, sessions: map[string]*Session{}}
}

// Devices proxies the hub's device list.
func (b *Broker) Devices(agentFilter []string) []relay.DeviceView {
	return b.hub.Devices(agentFilter)
}

// reapGrace is how long a dead session stays visible so its final buffered
// bytes can be drained before the broker forgets it.
const reapGrace = 2 * time.Minute

// Open opens a device and starts the receive pump. owner identifies the
// opening credential; only that owner can access the session afterwards.
func (b *Broker) Open(deviceID string, params proto.OpenParams, agentFilter []string, owner string) (*Session, error) {
	if params.Baud == 0 {
		params.Baud = 115200
	}
	st, err := b.hub.Open(deviceID, params, agentFilter)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:       st.SessionID,
		AgentID:  st.AgentID,
		DeviceID: st.DeviceID,
		Opened:   time.Now(),
		Owner:    owner,
		stream:   st,
	}
	s.cond = sync.NewCond(&s.mu)
	b.mu.Lock()
	b.sessions[s.ID] = s
	b.mu.Unlock()
	go func() {
		s.pump()
		// The stream is dead (agent gone or closed). Keep the entry around
		// briefly for a final drain, then reap so ghost sessions don't
		// accumulate when consumers vanish without closing.
		time.Sleep(reapGrace)
		b.mu.Lock()
		if cur, ok := b.sessions[s.ID]; ok && cur == s {
			delete(b.sessions, s.ID)
		}
		b.mu.Unlock()
	}()
	return s, nil
}

// pump moves bytes device→buffer. When the buffer is full it stops reading,
// letting yamux flow control push backpressure to the agent.
func (s *Session) pump() {
	chunk := make([]byte, 32*1024)
	for {
		s.mu.Lock()
		for s.buf.Len() >= sessionBufCap && !s.closed {
			s.cond.Wait()
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		n, err := s.stream.Conn.Read(chunk)
		s.mu.Lock()
		if n > 0 {
			s.buf.Write(chunk[:n])
			s.bytesIn += uint64(n)
		}
		if err != nil {
			s.rdErr = err
			s.closed = true
		}
		s.cond.Broadcast()
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// ReadResult is what a bounded read returns.
type ReadResult struct {
	Data     []byte
	TimedOut bool
	EOF      bool
}

// Read returns buffered device output. It blocks until at least one byte is
// available (or, with delimiter, until the delimiter arrives), the timeout
// elapses, or the session ends. Never indefinite: timeout is clamped.
func (s *Session) Read(timeout time.Duration, maxBytes int, delimiter []byte) ReadResult {
	if maxBytes <= 0 {
		maxBytes = DefaultReadMax
	}
	maxBytes = min(maxBytes, MaxReadBytes)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	timeout = min(timeout, MaxReadTimeout)
	deadline := time.Now().Add(timeout)

	// Wake sleepers when the deadline passes.
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if have := s.takeLocked(maxBytes, delimiter); have != nil {
			return ReadResult{Data: have}
		}
		if s.closed {
			// Session over: return whatever remains.
			rest := s.buf.Next(maxBytes)
			return ReadResult{Data: rest, EOF: s.buf.Len() == 0}
		}
		if !time.Now().Before(deadline) {
			// Timeout: partial data is still data.
			rest := s.buf.Next(maxBytes)
			return ReadResult{Data: rest, TimedOut: true}
		}
		s.cond.Wait()
	}
}

// takeLocked returns a completed read per the delimiter rules, or nil if the
// read should keep waiting.
func (s *Session) takeLocked(maxBytes int, delimiter []byte) []byte {
	if len(delimiter) > 0 {
		if i := bytes.Index(s.buf.Bytes(), delimiter); i >= 0 && i+len(delimiter) <= maxBytes {
			out := s.buf.Next(i + len(delimiter))
			s.cond.Broadcast() // pump may be waiting on space
			return out
		}
		if s.buf.Len() >= maxBytes {
			out := s.buf.Next(maxBytes) // delimiter never fit; don't stall
			s.cond.Broadcast()
			return out
		}
		return nil
	}
	if s.buf.Len() > 0 {
		out := s.buf.Next(maxBytes)
		s.cond.Broadcast()
		return out
	}
	return nil
}

// Write sends bytes to the device.
func (s *Session) Write(data []byte) (int, error) {
	if len(data) > MaxWriteBytes {
		return 0, fmt.Errorf("write exceeds %d bytes", MaxWriteBytes)
	}
	n, err := s.stream.Raw.Write(data)
	s.mu.Lock()
	s.bytesOut += uint64(n)
	s.mu.Unlock()
	return n, err
}

// Counters returns bytes device→consumer, consumer→device.
func (s *Session) Counters() (in, out uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesIn, s.bytesOut
}

// ErrUnknownSession is returned for a session ID the broker doesn't hold.
var ErrUnknownSession = errors.New("unknown session")

// Get looks up a live session.
func (b *Broker) Get(id string) (*Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[id]; ok {
		return s, nil
	}
	return nil, ErrUnknownSession
}

// Sessions lists live sessions, oldest first.
func (b *Broker) Sessions() []*Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*Session, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Opened.Before(out[j].Opened) })
	return out
}

// SetParams forwards line-parameter changes (subject to the device's grant,
// enforced agent-side).
func (b *Broker) SetParams(id string, baud *int, dtr, rts *bool) error {
	s, err := b.Get(id)
	if err != nil {
		return err
	}
	res := b.hub.SetParams(s.AgentID, s.ID, baud, dtr, rts)
	if !res.OK {
		return errors.New(res.Reason)
	}
	return nil
}

// Drain forwards a drain request.
func (b *Broker) Drain(id string) error {
	s, err := b.Get(id)
	if err != nil {
		return err
	}
	res := b.hub.Drain(s.AgentID, s.ID)
	if !res.OK {
		return errors.New(res.Reason)
	}
	return nil
}

// Close ends a session and notifies the agent.
func (b *Broker) Close(id string) error {
	b.mu.Lock()
	s, ok := b.sessions[id]
	delete(b.sessions, id)
	b.mu.Unlock()
	if !ok {
		return ErrUnknownSession
	}
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	s.stream.Raw.Close()
	b.hub.NotifyClosed(s.AgentID, s.ID)
	return nil
}
