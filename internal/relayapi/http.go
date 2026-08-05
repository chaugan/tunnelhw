package relayapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chaugan/tunnelhw/internal/auth"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/relay"
)

// API is the HTTP surface over the broker: versioned under /api/v1, bearer
// authenticated, one JSON object in and out per call.
type API struct {
	Broker *Broker
	Auth   *auth.Store
}

// Handler returns the /api/v1 mux.
func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/devices", a.withAuth(false, a.devices))
	m.HandleFunc("GET /api/v1/sessions", a.withAuth(false, a.listSessions))
	m.HandleFunc("POST /api/v1/sessions", a.withAuth(true, a.openSession))
	m.HandleFunc("POST /api/v1/sessions/{id}/read", a.withAuth(false, a.read))
	m.HandleFunc("POST /api/v1/sessions/{id}/write", a.withAuth(true, a.write))
	m.HandleFunc("POST /api/v1/sessions/{id}/params", a.withAuth(true, a.setParams))
	m.HandleFunc("POST /api/v1/sessions/{id}/drain", a.withAuth(true, a.drain))
	m.HandleFunc("DELETE /api/v1/sessions/{id}", a.withAuth(true, a.closeSession))
	return m
}

func (a *API) withAuth(needsWrite bool, next func(http.ResponseWriter, *http.Request, *auth.APIToken)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			jsonErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tok, ok := a.Auth.VerifyAPIToken(strings.TrimPrefix(h, prefix))
		if !ok {
			jsonErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if needsWrite && tok.ReadOnly {
			jsonErr(w, http.StatusForbidden, "token is read-only")
			return
		}
		next(w, r, tok)
	}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func (a *API) devices(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	devs := a.Broker.Devices(tok.Agents)
	if devs == nil {
		devs = []relay.DeviceView{}
	}
	jsonOut(w, http.StatusOK, map[string]any{"devices": devs})
}

type openReq struct {
	DeviceID string           `json:"device_id"`
	Params   proto.OpenParams `json:"params"`
}

func (a *API) openSession(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	var req openReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.DeviceID == "" {
		jsonErr(w, http.StatusBadRequest, "device_id is required")
		return
	}
	s, err := a.Broker.Open(req.DeviceID, req.Params, tok.Agents)
	if err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	jsonOut(w, http.StatusOK, s)
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	type sview struct {
		*Session
		BytesIn  uint64 `json:"bytes_in"`
		BytesOut uint64 `json:"bytes_out"`
	}
	out := []sview{}
	for _, s := range a.Broker.Sessions() {
		if len(tok.Agents) > 0 && !containsStr(tok.Agents, s.AgentID) {
			continue
		}
		in, o := s.Counters()
		out = append(out, sview{Session: s, BytesIn: in, BytesOut: o})
	}
	jsonOut(w, http.StatusOK, map[string]any{"sessions": out})
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sessionFor enforces token agent-scoping on session access.
func (a *API) sessionFor(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) *Session {
	s, err := a.Broker.Get(r.PathValue("id"))
	if err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return nil
	}
	if len(tok.Agents) > 0 && !containsStr(tok.Agents, s.AgentID) {
		jsonErr(w, http.StatusNotFound, ErrUnknownSession.Error())
		return nil
	}
	return s
}

type readReq struct {
	TimeoutMs int    `json:"timeout_ms"`
	MaxBytes  int    `json:"max_bytes"`
	Delimiter string `json:"delimiter"` // e.g. "\n"; empty = any data
}

func (a *API) read(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	s := a.sessionFor(w, r, tok)
	if s == nil {
		return
	}
	var req readReq
	if !decodeBody(w, r, &req) {
		return
	}
	res := s.Read(time.Duration(req.TimeoutMs)*time.Millisecond, req.MaxBytes, []byte(req.Delimiter))
	out := map[string]any{
		"data_b64":  base64.StdEncoding.EncodeToString(res.Data),
		"timed_out": res.TimedOut,
		"eof":       res.EOF,
		"n":         len(res.Data),
	}
	if utf8.Valid(res.Data) {
		out["text"] = string(res.Data)
	}
	jsonOut(w, http.StatusOK, out)
}

type writeReq struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding"` // utf8 (default) | base64
}

func (a *API) write(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	s := a.sessionFor(w, r, tok)
	if s == nil {
		return
	}
	var req writeReq
	if !decodeBody(w, r, &req) {
		return
	}
	var payload []byte
	switch req.Encoding {
	case "", "utf8":
		payload = []byte(req.Data)
	case "base64":
		var err error
		payload, err = base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "bad base64: "+err.Error())
			return
		}
	default:
		jsonErr(w, http.StatusBadRequest, "encoding must be utf8 or base64")
		return
	}
	n, err := s.Write(payload)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"written": n})
}

type paramsReq struct {
	Baud *int  `json:"baud,omitempty"`
	DTR  *bool `json:"dtr,omitempty"`
	RTS  *bool `json:"rts,omitempty"`
}

func (a *API) setParams(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	s := a.sessionFor(w, r, tok)
	if s == nil {
		return
	}
	var req paramsReq
	if !decodeBody(w, r, &req) {
		return
	}
	if err := a.Broker.SetParams(s.ID, req.Baud, req.DTR, req.RTS); err != nil {
		jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) drain(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	s := a.sessionFor(w, r, tok)
	if s == nil {
		return
	}
	if err := a.Broker.Drain(s.ID); err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) closeSession(w http.ResponseWriter, r *http.Request, tok *auth.APIToken) {
	s := a.sessionFor(w, r, tok)
	if s == nil {
		return
	}
	if err := a.Broker.Close(s.ID); err != nil && !errors.Is(err, ErrUnknownSession) {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}
