// Package mcp is the MCP adapter over the relay API: a bearer-authenticated
// streamable-HTTP endpoint whose tools map 1:1 onto relayapi.Broker. It holds
// no business logic: guardrails (bounded reads, write caps, exclusive open)
// live in the broker; this package only translates tool calls and enforces
// the token's agent scope and read-only flag.
//
// Auth is abstracted behind a verify callback so this package does not import
// internal/auth: the caller supplies token verification, this package supplies
// the 401 surface and per-request tool wiring.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/relayapi"
)

// Version is reported to MCP clients. The binary sets it from its build
// version; a literal here would drift every release.
var Version = "dev"

// grant is the verified capability of one request's bearer token. owner is
// the token's identity (its SHA-256 hex); sessions are only visible to the
// credential that opened them.
type grant struct {
	agents   []string // empty = all agents
	readOnly bool
	owner    string
}

type grantKey struct{}

// Handler returns the streamable-HTTP MCP endpoint over the broker. Every
// request must carry "Authorization: Bearer <token>"; verify maps the token
// to an agent filter (empty = all agents) and a read-only flag. Bad tokens
// get a 401 with a JSON error body.
//
// The handler runs the MCP server in stateless mode: each request is
// authenticated and served independently, so a token revoked between calls
// takes effect immediately.
func Handler(b *relayapi.Broker, verify func(token string) (agentFilter []string, readOnly bool, ok bool)) http.Handler {
	streamable := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		g, ok := r.Context().Value(grantKey{}).(*grant)
		if !ok || g == nil {
			// Unreachable through the auth wrapper below; if a refactor ever
			// mounts the streamable handler bare, fail CLOSED, not open.
			return newServer(b, grant{readOnly: true, agents: []string{"\x00deny-all"}, owner: "\x00deny-all"})
		}
		return newServer(b, *g)
	}, &sdk.StreamableHTTPOptions{Stateless: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			jsonErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		raw := strings.TrimPrefix(h, prefix)
		agents, readOnly, ok := verify(raw)
		if !ok {
			jsonErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		// The owner key is the token's SHA-256 hex, the same identity the
		// auth store records, so HTTP-API and MCP sessions share ownership.
		sum := sha256.Sum256([]byte(raw))
		g := &grant{agents: agents, readOnly: readOnly, owner: hex.EncodeToString(sum[:])}
		ctx := context.WithValue(r.Context(), grantKey{}, g)
		streamable.ServeHTTP(w, r.WithContext(ctx))
	})
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Shared description fragments the LLM must see on every stateful tool.
const (
	sessionResetNote = " IMPORTANT: sessions do NOT survive an agent reconnect; if the agent's " +
		"tunnel drops and comes back, all session IDs are invalid and writes are never " +
		"buffered or replayed; call open_device again and re-establish state yourself."
	exclusiveNote = " Devices are exclusive-open: only one session may hold a device at a " +
		"time, and an open attempt on a busy device fails with a deterministic 'busy' " +
		"error naming the current holder."
)

// newServer builds a per-request MCP server whose tools are closed over the
// request's verified grant.
func newServer(b *relayapi.Broker, g grant) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{
		Name:    "tunnelhw",
		Title:   "TunnelHW remote hardware",
		Version: Version,
	}, nil)

	sdk.AddTool(s, &sdk.Tool{
		Name: "list_devices",
		Description: "List the hardware devices currently exposed by connected agents. " +
			"Each device has a stable human-readable word-pair id (e.g. 'amber-falcon') " +
			"scoped to its agent; use that id with open_device. 'online' means the " +
			"owning agent is connected; 'busy' means another session already holds the " +
			"device." + exclusiveNote + " 'fingerprint_confidence' (strong/medium/weak) " +
			"says how reliably the id sticks to the same physical device across replugs. " +
			"'control_lines_allowed' says whether set_params may toggle DTR/RTS on it.",
	}, listDevices(b, g))

	sdk.AddTool(s, &sdk.Tool{
		Name: "open_device",
		Description: "Open an exclusive session on a device and return its session_id for " +
			"read/write/set_params/drain/close_session." + exclusiveNote +
			" Serial parameters default to 115200 8-N-1; pass baud/data_bits/parity/stop_bits " +
			"to override at open. Opening does NOT touch the DTR/RTS control lines, so " +
			"attaching to a board wired for auto-reset (most ESP32 and Arduino boards) " +
			"will not reboot it and will not disturb the state you are trying to observe. " +
			"Pass dtr/rts false to state that intent explicitly." + sessionResetNote +
			" Always call close_session when done: sessions are not garbage-collected while " +
			"the agent stays connected, and a forgotten session keeps the device busy for everyone.",
	}, openDevice(b, g))

	sdk.AddTool(s, &sdk.Tool{
		Name: "read",
		Description: "Read buffered output from an open session. Bounded and never " +
			"indefinitely blocking: waits up to timeout_ms (default 2000, max 55000) for " +
			"data, returns at most max_bytes (default 4096, max 262144). The 55000 ceiling " +
			"leaves headroom under the 60s call limit most MCP clients impose, so a long " +
			"read returns an answer instead of failing as a transport timeout. Set lines " +
			"true for log-shaped output to get only whole lines, with any partial tail " +
			"held over. With delimiter " +
			"set (e.g. \"\\n\"), returns once a chunk ending in the delimiter is available, " +
			"or whatever has arrived when the timeout fires; partial reads are normal, " +
			"check 'timed_out' and call read again for more. 'text' is present only when " +
			"the bytes are valid UTF-8; 'data_b64' always carries the exact bytes. " +
			"'eof' true means the session is over." + sessionResetNote,
	}, readTool(b, g))

	sdk.AddTool(s, &sdk.Tool{
		Name: "write",
		Description: "Write bytes to an open session. encoding 'utf8' (default) sends the " +
			"string as-is; use 'base64' for binary payloads. Writes above 262144 bytes are " +
			"rejected. Writes are never buffered across a disconnect: if the agent " +
			"reconnects mid-operation the session is gone and nothing is replayed. " +
			"Note that if the device's own firmware is reading the same UART it will " +
			"consume replies before you see them, so writing is not a reliable probe " +
			"on a device that is busy talking to itself." + sessionResetNote,
	}, writeTool(b, g))

	sdk.AddTool(s, &sdk.Tool{
		Name: "set_params",
		Description: "Change line parameters on an open session: baud rate and/or the DTR/RTS " +
			"control lines. ALL of these (including a baud-only change) require the device's " +
			"per-device control-lines grant (see 'control_lines_allowed' in list_devices); " +
			"without it the call fails. (Setting the baud at open_device time needs no grant; " +
			"only changing it mid-session does.) Toggling DTR/RTS can physically reset a board " +
			"or put it into its bootloader. Omitted fields are left unchanged." + sessionResetNote,
	}, setParamsTool(b, g))

	sdk.AddTool(s, &sdk.Tool{
		Name: "drain",
		Description: "Flush the device's transmit buffer: blocks until bytes already written " +
			"to the session have been handed to the hardware. Use before toggling control " +
			"lines or closing when the last write must actually reach the device." +
			sessionResetNote,
	}, drainTool(b, g))

	sdk.AddTool(s, &sdk.Tool{
		Name: "close_session",
		Description: "Close an open session and release the device for other consumers. " +
			"Explicit close is required: always call this when finished with a device. " +
			"Closing an already-gone session returns an 'unknown session' error, which is " +
			"safe to ignore.",
	}, closeTool(b, g))

	return s
}

// errReadOnly is the tool error for mutating calls under a read-only token.
func errReadOnly(tool string) error {
	return fmt.Errorf("token is read-only: %s is forbidden", tool)
}

// sessionFor resolves a session ID under the grant's ownership: sessions
// opened by any other credential are indistinguishable from unknown ones.
func sessionFor(b *relayapi.Broker, g grant, id string) (*relayapi.Session, error) {
	s, err := b.Get(id)
	if err != nil || s.Owner != g.owner {
		return nil, relayapi.ErrUnknownSession
	}
	return s, nil
}

type emptyIn struct{}

type deviceInfo struct {
	AgentID             string `json:"agent_id" jsonschema:"id of the agent (machine) hosting the device"`
	AgentName           string `json:"agent_name,omitempty" jsonschema:"human-readable agent name, if set"`
	ID                  string `json:"id" jsonschema:"word-pair device id (pass this to open_device)"`
	Class               string `json:"class" jsonschema:"device class; 'serial' in v1"`
	Transport           string `json:"transport" jsonschema:"how the port attaches: usb, native, bluetooth, ..."`
	Path                string `json:"path" jsonschema:"OS port path, e.g. /dev/ttyUSB0 or COM3"`
	Product             string `json:"product,omitempty" jsonschema:"product string reported by the hardware, if any"`
	Confidence          string `json:"fingerprint_confidence" jsonschema:"strong, medium, or weak: how reliably the id follows the physical device"`
	ControlLinesAllowed bool   `json:"control_lines_allowed" jsonschema:"whether set_params may toggle DTR/RTS on this device"`
	Online              bool   `json:"online" jsonschema:"owning agent is currently connected"`
	Busy                bool   `json:"busy" jsonschema:"another session holds the device right now"`
	ClaimedBy           string `json:"claimed_by,omitempty" jsonschema:"who holds the device, when busy"`
}

type listDevicesOut struct {
	Devices []deviceInfo `json:"devices"`
}

func listDevices(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[emptyIn, listDevicesOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in emptyIn) (*sdk.CallToolResult, listDevicesOut, error) {
		out := listDevicesOut{Devices: []deviceInfo{}}
		for _, v := range b.Devices(g.agents) {
			out.Devices = append(out.Devices, deviceInfo{
				AgentID:             v.AgentID,
				AgentName:           v.AgentName,
				ID:                  v.Device.ID,
				Class:               v.Device.Class,
				Transport:           v.Device.Meta.Transport,
				Path:                v.Device.Meta.Path,
				Product:             v.Device.Meta.Product,
				Confidence:          v.Device.Meta.FingerprintConfidence,
				ControlLinesAllowed: v.Device.Meta.ControlLinesAllowed,
				Online:              v.Device.Online,
				Busy:                v.Device.Busy,
				ClaimedBy:           v.Device.ClaimedBy,
			})
		}
		return nil, out, nil
	}
}

type openIn struct {
	DeviceID string `json:"device_id" jsonschema:"word-pair device id from list_devices"`
	Baud     int    `json:"baud,omitempty" jsonschema:"baud rate; default 115200"`
	DataBits int    `json:"data_bits,omitempty" jsonschema:"data bits; default 8"`
	Parity   string `json:"parity,omitempty" jsonschema:"none, odd, or even; default none"`
	StopBits string `json:"stop_bits,omitempty" jsonschema:"1, 1.5, or 2; default 1"`
	DTR      *bool  `json:"dtr,omitempty" jsonschema:"control the DTR line at open; false guarantees the line is not raised, which is what you want to observe a device without disturbing it"`
	RTS      *bool  `json:"rts,omitempty" jsonschema:"control the RTS line at open; false guarantees the line is not raised"`
}

type openOut struct {
	SessionID string `json:"session_id"`
}

func openDevice(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[openIn, openOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in openIn) (*sdk.CallToolResult, openOut, error) {
		if g.readOnly {
			return nil, openOut{}, errReadOnly("open_device")
		}
		if in.DeviceID == "" {
			return nil, openOut{}, fmt.Errorf("device_id is required")
		}
		s, err := b.Open(in.DeviceID, proto.OpenParams{
			Baud:     in.Baud,
			DataBits: in.DataBits,
			Parity:   in.Parity,
			StopBits: in.StopBits,
			DTR:      in.DTR,
			RTS:      in.RTS,
		}, g.agents, g.owner)
		if err != nil {
			return nil, openOut{}, err
		}
		return nil, openOut{SessionID: s.ID}, nil
	}
}

type readIn struct {
	SessionID string `json:"session_id" jsonschema:"session id from open_device"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"how long to wait for data in milliseconds; default 2000, max 55000 (values above are clamped)"`
	MaxBytes  int    `json:"max_bytes,omitempty" jsonschema:"maximum bytes to return; default 4096, max 262144"`
	Delimiter string `json:"delimiter,omitempty" jsonschema:"return once data ending in this string is available, e.g. \"\\n\""`
	Lines     bool   `json:"lines,omitempty" jsonschema:"return only whole lines, holding any partial trailing line for the next call; best for log-shaped output"`
}

type readOut struct {
	Text     string `json:"text,omitempty" jsonschema:"the bytes as a string; present only when valid UTF-8"`
	DataB64  string `json:"data_b64" jsonschema:"the exact bytes, base64-encoded"`
	N        int    `json:"n" jsonschema:"number of bytes returned"`
	TimedOut bool   `json:"timed_out" jsonschema:"the timeout fired; data may be partial or empty"`
	EOF      bool   `json:"eof" jsonschema:"the session is over and its buffer is empty"`
}

func readTool(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[readIn, readOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in readIn) (*sdk.CallToolResult, readOut, error) {
		s, err := sessionFor(b, g, in.SessionID)
		if err != nil {
			return nil, readOut{}, err
		}
		res := s.ReadWith(relayapi.ReadOptions{
			Timeout:   time.Duration(in.TimeoutMs) * time.Millisecond,
			MaxBytes:  in.MaxBytes,
			Delimiter: []byte(in.Delimiter),
			Lines:     in.Lines,
		})
		out := readOut{
			DataB64:  base64.StdEncoding.EncodeToString(res.Data),
			N:        len(res.Data),
			TimedOut: res.TimedOut,
			EOF:      res.EOF,
		}
		if utf8.Valid(res.Data) {
			out.Text = string(res.Data)
		}
		return nil, out, nil
	}
}

type writeIn struct {
	SessionID string `json:"session_id" jsonschema:"session id from open_device"`
	Data      string `json:"data" jsonschema:"payload to send"`
	Encoding  string `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64 for binary"`
}

type writeOut struct {
	Written int `json:"written" jsonschema:"number of bytes written"`
}

func writeTool(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[writeIn, writeOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in writeIn) (*sdk.CallToolResult, writeOut, error) {
		if g.readOnly {
			return nil, writeOut{}, errReadOnly("write")
		}
		s, err := sessionFor(b, g, in.SessionID)
		if err != nil {
			return nil, writeOut{}, err
		}
		var payload []byte
		switch in.Encoding {
		case "", "utf8":
			payload = []byte(in.Data)
		case "base64":
			payload, err = base64.StdEncoding.DecodeString(in.Data)
			if err != nil {
				return nil, writeOut{}, fmt.Errorf("bad base64: %w", err)
			}
		default:
			return nil, writeOut{}, fmt.Errorf("encoding must be utf8 or base64")
		}
		n, err := s.Write(payload)
		if err != nil {
			return nil, writeOut{}, err
		}
		return nil, writeOut{Written: n}, nil
	}
}

type setParamsIn struct {
	SessionID string `json:"session_id" jsonschema:"session id from open_device"`
	Baud      *int   `json:"baud,omitempty" jsonschema:"new baud rate; omit to leave unchanged"`
	DTR       *bool  `json:"dtr,omitempty" jsonschema:"set the DTR line; requires the device's control-lines grant"`
	RTS       *bool  `json:"rts,omitempty" jsonschema:"set the RTS line; requires the device's control-lines grant"`
}

type okOut struct {
	OK bool `json:"ok"`
}

func setParamsTool(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[setParamsIn, okOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in setParamsIn) (*sdk.CallToolResult, okOut, error) {
		if g.readOnly {
			return nil, okOut{}, errReadOnly("set_params")
		}
		s, err := sessionFor(b, g, in.SessionID)
		if err != nil {
			return nil, okOut{}, err
		}
		if err := b.SetParams(s.ID, in.Baud, in.DTR, in.RTS); err != nil {
			return nil, okOut{}, err
		}
		return nil, okOut{OK: true}, nil
	}
}

type sessionIn struct {
	SessionID string `json:"session_id" jsonschema:"session id from open_device"`
}

func drainTool(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[sessionIn, okOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in sessionIn) (*sdk.CallToolResult, okOut, error) {
		if g.readOnly {
			return nil, okOut{}, errReadOnly("drain")
		}
		s, err := sessionFor(b, g, in.SessionID)
		if err != nil {
			return nil, okOut{}, err
		}
		if err := b.Drain(s.ID); err != nil {
			return nil, okOut{}, err
		}
		return nil, okOut{OK: true}, nil
	}
}

func closeTool(b *relayapi.Broker, g grant) sdk.ToolHandlerFor[sessionIn, okOut] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in sessionIn) (*sdk.CallToolResult, okOut, error) {
		if g.readOnly {
			return nil, okOut{}, errReadOnly("close_session")
		}
		s, err := sessionFor(b, g, in.SessionID)
		if err != nil {
			return nil, okOut{}, err
		}
		if err := b.Close(s.ID); err != nil {
			return nil, okOut{}, err
		}
		return nil, okOut{OK: true}, nil
	}
}
