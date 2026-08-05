// Package agent implements the local agent: device registry, expose-list,
// exclusive device sessions, and the tunnel client toward the relay.
package agent

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chaugan/tunnelhw/internal/config"
	"github.com/chaugan/tunnelhw/internal/names"
	"github.com/chaugan/tunnelhw/internal/proto"
	"github.com/chaugan/tunnelhw/internal/serialdev"
	"github.com/google/uuid"
)

// MaxSessions caps concurrent open device sessions (abuse limit).
const MaxSessions = 16

// deviceState joins durable identity (config record) with live presence.
type deviceState struct {
	FP     serialdev.Fingerprint
	Info   serialdev.PortInfo
	Rec    config.DeviceRecord
	Online bool
}

// Core is the agent's brain. All methods are safe for concurrent use.
type Core struct {
	mu        sync.Mutex
	dir       string
	cfg       *config.Config
	enumerate func() ([]serialdev.PortInfo, error)
	open      serialdev.Opener

	devices  map[string]*deviceState // key: fingerprint key
	sessions map[string]*Session     // key: session id
	claims   map[string]string       // device uuid -> session id

	activity *Activity
	onChange func() // fires after any state change worth re-announcing
}

// New builds a Core. enumerate/open are injectable for tests; pass
// serialdev.Enumerate and serialdev.Open in production.
func New(dir string, cfg *config.Config, enumerate func() ([]serialdev.PortInfo, error), open serialdev.Opener) *Core {
	return &Core{
		dir:       dir,
		cfg:       cfg,
		enumerate: enumerate,
		open:      open,
		devices:   map[string]*deviceState{},
		sessions:  map[string]*Session{},
		claims:    map[string]string{},
		activity:  newActivity(256),
	}
}

// OnChange registers the re-announce hook (single consumer).
func (c *Core) OnChange(fn func()) { c.onChange = fn }

func (c *Core) changed() {
	if c.onChange != nil {
		go c.onChange()
	}
}

// Activity returns the live activity log.
func (c *Core) Activity() *Activity { return c.activity }

// Rescan enumerates hardware and reconciles it with the durable identity
// map, assigning UUIDs and word-IDs to devices seen for the first time.
// Known-but-absent devices stay in the registry as offline so their
// word-IDs remain stable.
func (c *Core) Rescan() error {
	ports, err := c.enumerate()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	dirty := false
	present := map[string]bool{}
	for _, p := range ports {
		fp := serialdev.FingerprintOf(p)
		present[fp.Key] = true
		rec, known := c.cfg.Devices[fp.Key]
		if !known {
			wordID, err := names.Generate(c.wordIDTakenLocked)
			if err != nil {
				return err
			}
			rec = config.DeviceRecord{UUID: uuid.NewString(), WordID: wordID}
			c.cfg.Devices[fp.Key] = rec
			dirty = true
			c.activity.Add("info", fmt.Sprintf("new device %s (%s, %s confidence) named %s", p.Path, fp.Transport, fp.Confidence, wordID))
		}
		c.devices[fp.Key] = &deviceState{FP: fp, Info: p, Rec: rec, Online: true}
	}
	for key, ds := range c.devices {
		if !present[key] {
			ds.Online = false
		}
	}
	if dirty {
		if err := config.Save(c.dir, c.cfg); err != nil {
			return err
		}
	}
	c.changed()
	return nil
}

func (c *Core) wordIDTakenLocked(id string) bool {
	for _, rec := range c.cfg.Devices {
		if rec.WordID == id {
			return true
		}
	}
	return false
}

// byUUIDLocked finds a device state by config UUID.
func (c *Core) byUUIDLocked(id string) (string, *deviceState) {
	for key, ds := range c.devices {
		if ds.Rec.UUID == id {
			return key, ds
		}
	}
	// Known in config but never seen this run.
	for key, rec := range c.cfg.Devices {
		if rec.UUID == id {
			if ds, ok := c.devices[key]; ok {
				return key, ds
			}
			ds := &deviceState{Rec: rec, Online: false}
			c.devices[key] = ds
			return key, ds
		}
	}
	return "", nil
}

// SetExposed toggles exposure for a device by UUID.
func (c *Core) SetExposed(uuid string, exposed bool) error {
	return c.updateRec(uuid, func(r *config.DeviceRecord) { r.Exposed = exposed })
}

// SetControlLines toggles the privileged control-lines/baud grant.
func (c *Core) SetControlLines(uuid string, allowed bool) error {
	return c.updateRec(uuid, func(r *config.DeviceRecord) { r.AllowControlLines = allowed })
}

// RegenerateName assigns a fresh word-ID to a device.
func (c *Core) RegenerateName(uuid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ds := c.byUUIDLocked(uuid)
	if ds == nil {
		return fmt.Errorf("agent: unknown device %s", uuid)
	}
	wordID, err := names.Generate(c.wordIDTakenLocked)
	if err != nil {
		return err
	}
	rec := c.cfg.Devices[key]
	old := rec.WordID
	rec.WordID = wordID
	c.cfg.Devices[key] = rec
	ds.Rec = rec
	if err := config.Save(c.dir, c.cfg); err != nil {
		return err
	}
	c.activity.Add("info", fmt.Sprintf("device %s renamed to %s", old, wordID))
	c.changed()
	return nil
}

func (c *Core) updateRec(uuid string, mut func(*config.DeviceRecord)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ds := c.byUUIDLocked(uuid)
	if ds == nil {
		return fmt.Errorf("agent: unknown device %s", uuid)
	}
	rec := c.cfg.Devices[key]
	mut(&rec)
	c.cfg.Devices[key] = rec
	ds.Rec = rec
	if err := config.Save(c.dir, c.cfg); err != nil {
		return err
	}
	c.changed()
	return nil
}

// ExposedDevices is the announce set: exposed devices only — the relay never
// learns about hidden ones.
func (c *Core) ExposedDevices() []proto.Device {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []proto.Device
	for _, ds := range c.devices {
		if !ds.Rec.Exposed {
			continue
		}
		sid, busy := c.claims[ds.Rec.UUID]
		_ = sid
		out = append(out, proto.Device{
			ID:     ds.Rec.WordID,
			UUID:   ds.Rec.UUID,
			Class:  "serial",
			Online: ds.Online,
			Busy:   busy,
			Meta: proto.DeviceMeta{
				Path:                  ds.Info.Path,
				Transport:             ds.FP.Transport,
				VID:                   ds.Info.VID,
				PID:                   ds.Info.PID,
				SerialNumber:          ds.Info.SerialNumber,
				Product:               ds.Info.Product,
				FingerprintConfidence: ds.FP.Confidence,
				ControlLinesAllowed:   ds.Rec.AllowControlLines,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// UIDevice is the local web UI's view (includes hidden devices).
type UIDevice struct {
	proto.Device
	Exposed bool `json:"exposed"`
}

// UIDevices lists every known device for the local UI.
func (c *Core) UIDevices() []UIDevice {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []UIDevice
	for _, ds := range c.devices {
		_, busy := c.claims[ds.Rec.UUID]
		out = append(out, UIDevice{
			Device: proto.Device{
				ID:     ds.Rec.WordID,
				UUID:   ds.Rec.UUID,
				Class:  "serial",
				Online: ds.Online,
				Busy:   busy,
				Meta: proto.DeviceMeta{
					Path:                  ds.Info.Path,
					Transport:             ds.FP.Transport,
					VID:                   ds.Info.VID,
					PID:                   ds.Info.PID,
					SerialNumber:          ds.Info.SerialNumber,
					Product:               ds.Info.Product,
					FingerprintConfidence: ds.FP.Confidence,
					ControlLinesAllowed:   ds.Rec.AllowControlLines,
				},
			},
			Exposed: ds.Rec.Exposed,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Session is one exclusive open of a device.
type Session struct {
	ID         string
	DeviceUUID string
	DeviceID   string
	Port       serialdev.Port
	Opened     time.Time

	mu               sync.Mutex
	bytesIn, bytesOut uint64
}

// Count adds to the session byte counters (metadata only — payloads are
// never logged).
func (s *Session) Count(in, out int) {
	s.mu.Lock()
	s.bytesIn += uint64(in)
	s.bytesOut += uint64(out)
	s.mu.Unlock()
}

// Counters returns bytes device→consumer, consumer→device.
func (s *Session) Counters() (uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesIn, s.bytesOut
}

// OpenSession opens a device by word-ID, enforcing exposure, exclusivity,
// and the session cap.
func (c *Core) OpenSession(deviceID string, params proto.OpenParams) (*Session, *proto.OpenResponse) {
	c.mu.Lock()
	var target *deviceState
	for _, ds := range c.devices {
		if ds.Rec.WordID == deviceID {
			target = ds
			break
		}
	}
	fail := func(reason string, busy bool, claimedBy string) (*Session, *proto.OpenResponse) {
		c.mu.Unlock()
		return nil, &proto.OpenResponse{OK: false, Reason: reason, Busy: busy, ClaimedBy: claimedBy}
	}
	if target == nil || !target.Rec.Exposed {
		// Identical answer for unknown and hidden: don't leak existence.
		return fail("unknown device "+deviceID, false, "")
	}
	if !target.Online {
		return fail("device "+deviceID+" is offline", false, "")
	}
	if sid, busy := c.claims[target.Rec.UUID]; busy {
		return fail("device "+deviceID+" is busy", true, sid)
	}
	if len(c.sessions) >= MaxSessions {
		return fail(fmt.Sprintf("session limit (%d) reached", MaxSessions), false, "")
	}
	path := target.Info.Path
	devUUID := target.Rec.UUID
	// Claim before releasing the lock so a concurrent open sees busy while
	// the (slow) hardware open happens outside the lock.
	sid := uuid.NewString()
	c.claims[devUUID] = sid
	c.mu.Unlock()

	port, err := c.open(path, params)
	if err != nil {
		c.mu.Lock()
		delete(c.claims, devUUID)
		c.mu.Unlock()
		c.changed()
		return nil, &proto.OpenResponse{OK: false, Reason: err.Error()}
	}
	s := &Session{ID: sid, DeviceUUID: devUUID, DeviceID: deviceID, Port: port, Opened: time.Now()}
	c.mu.Lock()
	c.sessions[sid] = s
	c.mu.Unlock()
	c.activity.Add("open", fmt.Sprintf("session %s opened %s at %d baud", short(sid), deviceID, params.Baud))
	c.changed()
	return s, &proto.OpenResponse{OK: true, SessionID: sid}
}

// CloseSession releases a device session.
func (c *Core) CloseSession(sid, reason string) {
	c.mu.Lock()
	s, ok := c.sessions[sid]
	if ok {
		delete(c.sessions, sid)
		delete(c.claims, s.DeviceUUID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	s.Port.Close()
	in, out := s.Counters()
	c.activity.Add("close", fmt.Sprintf("session %s on %s closed (%s) — %dB in / %dB out", short(sid), s.DeviceID, reason, in, out))
	c.changed()
}

// CloseAll is the kill switch: every session is severed.
func (c *Core) CloseAll(reason string) {
	c.mu.Lock()
	ids := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.CloseSession(id, reason)
	}
}

// SetParams applies line-parameter changes; DTR/RTS/baud need the device's
// control-lines grant, and every use is logged loudly.
func (c *Core) SetParams(sid string, baud *int, dtr, rts *bool) proto.Result {
	c.mu.Lock()
	s, ok := c.sessions[sid]
	var allowed bool
	if ok {
		for _, ds := range c.devices {
			if ds.Rec.UUID == s.DeviceUUID {
				allowed = ds.Rec.AllowControlLines
				break
			}
		}
	}
	c.mu.Unlock()
	if !ok {
		return proto.Result{OK: false, Reason: "unknown session"}
	}
	if (baud != nil || dtr != nil || rts != nil) && !allowed {
		c.activity.Add("denied", fmt.Sprintf("session %s: control-line/baud change DENIED on %s (no grant)", short(sid), s.DeviceID))
		return proto.Result{OK: false, Reason: "control lines and baud changes are not granted for " + s.DeviceID}
	}
	if err := s.Port.SetParams(baud, dtr, rts); err != nil {
		return proto.Result{OK: false, Reason: err.Error()}
	}
	c.activity.Add("control", fmt.Sprintf("session %s: params changed on %s (baud=%v dtr=%v rts=%v)", short(sid), s.DeviceID, deref(baud), deref(dtr), deref(rts)))
	return proto.Result{OK: true}
}

// Drain flushes pending output on a session.
func (c *Core) Drain(sid string) proto.Result {
	c.mu.Lock()
	s, ok := c.sessions[sid]
	c.mu.Unlock()
	if !ok {
		return proto.Result{OK: false, Reason: "unknown session"}
	}
	if err := s.Port.Drain(); err != nil {
		return proto.Result{OK: false, Reason: err.Error()}
	}
	return proto.Result{OK: true}
}

// Sessions snapshots live sessions for the UI.
func (c *Core) Sessions() []*Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Session, 0, len(c.sessions))
	for _, s := range c.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Opened.Before(out[j].Opened) })
	return out
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func deref[T any](p *T) any {
	if p == nil {
		return "-"
	}
	return *p
}
