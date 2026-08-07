// Package proto defines the TunnelHW control protocol spoken between the
// agent and the relay over a multiplexed session.
//
// Framing: the first stream the agent opens after the yamux session is
// established is the control stream. Control messages are newline-delimited
// JSON envelopes, size-capped. Device data streams are separate yamux streams
// whose first frame is a JSON open-header line; after the header exchange the
// stream carries raw bytes both ways.
package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Supported protocol versions of this build, ascending. Version negotiation:
// the agent's hello carries every version it supports; the relay picks the
// highest version both sides share, and closes with hello_err if there is
// no overlap at or above VersionFloor.
const (
	VersionFloor = 1
	Version      = 1
)

// MaxControlMessage caps a single control-stream or open-header frame.
// Anything larger is a protocol violation, not a bigger buffer.
const MaxControlMessage = 64 * 1024

// Control message types.
const (
	TypeHello           = "hello"
	TypeHelloOK         = "hello_ok"
	TypeHelloErr        = "hello_err"
	TypeAnnounce        = "announce"
	TypePing            = "ping"
	TypePong            = "pong"
	TypeSetParams       = "set_params"
	TypeSetParamsResult = "set_params_result"
	TypeDrain           = "drain"
	TypeDrainResult     = "drain_result"
	TypeSessionClosed   = "session_closed"
	TypeError           = "error"
)

// Envelope wraps every control-stream message.
type Envelope struct {
	Type    string          `json:"type"`
	Corr    string          `json:"corr,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hello is sent by the agent as the first control message.
type Hello struct {
	AgentID       string `json:"agent_id"`
	Credential    string `json:"credential"`
	ProtoVersions []int  `json:"proto_versions"`
	AgentVersion  string `json:"agent_version,omitempty"`
}

// HelloOK is the relay's acceptance of a Hello.
type HelloOK struct {
	ProtoVersion int `json:"proto_version"`
	HeartbeatSec int `json:"heartbeat_sec"`
}

// HelloErr rejects a Hello; the connection closes after it.
type HelloErr struct {
	Reason string `json:"reason"`
}

// Fingerprint confidence tiers (see ARCHITECTURE.md §5.2).
const (
	ConfidenceStrong = "strong" // USB serial number
	ConfidenceMedium = "medium" // fixed native port identity (COM1, /dev/ttyS0)
	ConfidenceWeak   = "weak"   // VID:PID+path or OS-assigned hot-plug name
)

// Device transports.
const (
	TransportUSB       = "usb"
	TransportNative    = "native"
	TransportBluetooth = "bluetooth"
	TransportUnknown   = "unknown"
)

// DeviceMeta describes a device beyond its identity.
type DeviceMeta struct {
	Path                  string `json:"path"`
	Transport             string `json:"transport"`
	VID                   string `json:"vid,omitempty"`
	PID                   string `json:"pid,omitempty"`
	SerialNumber          string `json:"serial_number,omitempty"`
	Product               string `json:"product,omitempty"`
	FingerprintConfidence string `json:"fingerprint_confidence"`
	ControlLinesAllowed   bool   `json:"control_lines_allowed"`
	AssertLinesOnOpen     bool   `json:"assert_lines_on_open"`
	Monitored             bool   `json:"monitored"`
	// PortHeld reports that the agent currently holds the port open, so no
	// other application on that machine can use it.
	PortHeld bool `json:"port_held"`
	// Resets counts how many times the device has vanished and returned. A
	// consumer that sees this rise knows the device restarted.
	Resets int `json:"resets"`
}

// Device is one exposed device as announced to the relay. ID is the
// human-readable word-pair handle; UUID is the durable internal identity.
type Device struct {
	ID        string     `json:"id"`
	UUID      string     `json:"uuid"`
	Class     string     `json:"class"` // "serial" in v1
	Online    bool       `json:"online"`
	Busy      bool       `json:"busy"`
	ClaimedBy string     `json:"claimed_by,omitempty"`
	Meta      DeviceMeta `json:"meta"`
}

// Announce carries the full current set of exposed devices. It is sent on
// connect and again on every change; each announce replaces the previous set.
type Announce struct {
	Devices []Device `json:"devices"`
}

// SetParams asks the agent to change line parameters on an open session.
// Nil fields are left unchanged. DTR/RTS require the device's
// control-lines grant.
type SetParams struct {
	SessionID string `json:"session_id"`
	Baud      *int   `json:"baud,omitempty"`
	DTR       *bool  `json:"dtr,omitempty"`
	RTS       *bool  `json:"rts,omitempty"`
}

// Drain asks the agent to flush pending output on a session.
type Drain struct {
	SessionID string `json:"session_id"`
}

// Result is the generic ok/err response for correlated requests.
type Result struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// SessionClosed notifies the peer that a device session ended.
type SessionClosed struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

// OpenParams are the serial parameters requested at open.
type OpenParams struct {
	Baud     int    `json:"baud"`
	DataBits int    `json:"data_bits,omitempty"` // default 8
	Parity   string `json:"parity,omitempty"`    // none|odd|even (default none)
	StopBits string `json:"stop_bits,omitempty"` // 1|1.5|2 (default 1)

	// DTR and RTS state the caller's intent for the control lines at open.
	// nil means "use the device's policy". false is always honoured, because
	// leaving the lines alone can never disturb hardware. true requires the
	// device's control-lines grant, since raising them resets auto-reset
	// boards.
	DTR *bool `json:"dtr,omitempty"`
	RTS *bool `json:"rts,omitempty"`

	// AssertLinesOnOpen raises DTR and RTS as the port opens. It is set by
	// the agent from per-device policy and is deliberately NOT on the wire
	// (`json:"-"`): raising these lines resets boards wired for auto-reset,
	// so the decision belongs to the operator, never to the consumer.
	AssertLinesOnOpen bool `json:"-"`
}

// OpenRequest is the first frame on a new device data stream (relay → agent).
type OpenRequest struct {
	Corr     string     `json:"corr"`
	DeviceID string     `json:"device_id"`
	Params   OpenParams `json:"params"`
}

// OpenResponse is the reply frame. On OK the stream switches to raw bytes.
type OpenResponse struct {
	Corr      string `json:"corr"`
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Busy      bool   `json:"busy,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
}

// Negotiate picks the protocol version given the peer's supported list.
// It returns the highest version this build also supports, at or above
// VersionFloor.
func Negotiate(peerVersions []int) (int, error) {
	best := 0
	for _, v := range peerVersions {
		if v >= VersionFloor && v <= Version && v > best {
			best = v
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("no common protocol version (peer offers %v, this build supports %d..%d)", peerVersions, VersionFloor, Version)
	}
	return best, nil
}

var (
	// ErrMessageTooLarge is returned when a frame exceeds MaxControlMessage.
	ErrMessageTooLarge = errors.New("proto: message exceeds size cap")
)

// ReadFrame reads one newline-terminated JSON frame from br, enforcing the
// size cap. It returns the raw line without the trailing newline.
func ReadFrame(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxControlMessage {
			return nil, ErrMessageTooLarge
		}
		if err == nil {
			return buf[:len(buf)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

// Conn is a control-stream connection: concurrent-safe sends, sequential
// receives, size-capped frames.
type Conn struct {
	mu sync.Mutex
	w  io.Writer
	br *bufio.Reader
}

// NewConn wraps a raw stream as a control connection.
func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{w: rw, br: bufio.NewReaderSize(rw, 16*1024)}
}

// Send marshals payload into an envelope and writes one frame.
func (c *Conn) Send(typ, corr string, payload any) error {
	env := Envelope{Type: typ, Corr: corr}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		env.Payload = raw
	}
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(line) > MaxControlMessage {
		return ErrMessageTooLarge
	}
	line = append(line, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(line)
	return err
}

// Recv reads the next envelope. Not safe for concurrent use.
func (c *Conn) Recv() (Envelope, error) {
	var env Envelope
	line, err := ReadFrame(c.br)
	if err != nil {
		return env, err
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return env, fmt.Errorf("proto: bad frame: %w", err)
	}
	if env.Type == "" {
		return env, errors.New("proto: frame missing type")
	}
	return env, nil
}

// ReadInto reads one JSON header frame from the connection into v.
func (c *Conn) ReadInto(v any) error { return ReadHeaderFrame(c.br, v) }

// Read reads raw bytes, serving anything already buffered past a header
// frame first. Used when a stream switches from header exchange to raw data.
func (c *Conn) Read(p []byte) (int, error) { return c.br.Read(p) }

// Decode unmarshals an envelope payload into v.
func Decode[T any](env Envelope) (T, error) {
	var v T
	if len(env.Payload) == 0 {
		return v, fmt.Errorf("proto: %s: empty payload", env.Type)
	}
	err := json.Unmarshal(env.Payload, &v)
	return v, err
}

// WriteHeaderFrame writes a single JSON header line to w (used for the
// open-header exchange on device data streams).
func WriteHeaderFrame(w io.Writer, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(line) > MaxControlMessage {
		return ErrMessageTooLarge
	}
	line = append(line, '\n')
	_, err = w.Write(line)
	return err
}

// ReadHeaderFrame reads one JSON header line from br into v. The caller must
// keep using the same bufio.Reader afterwards for raw stream data: bytes
// beyond the newline may already be buffered.
func ReadHeaderFrame(br *bufio.Reader, v any) error {
	line, err := ReadFrame(br)
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}
