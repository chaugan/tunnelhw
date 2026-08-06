package agent

import (
	"bytes"
	"errors"
	"io"
	"sync"

	"github.com/chaugan/tunnelhw/internal/serialdev"
)

// A monitor holds a device's port open continuously and records what it says.
//
// It exists for two reasons.
//
// Some hardware reacts to its port being opened. Leaving the control lines
// alone at open is enough for the boards tested so far, but the port is opened
// by CreateFile on Windows before any line settings can be applied, so a
// driver default could still assert DTR ahead of us on hardware we have not
// seen. Holding the port open sidesteps the question: it is opened once, when
// monitoring starts, rather than on every session.
//
// Anything the device says between sessions is otherwise lost, which is
// exactly when a device that logs at boot is most interesting.
type monitor struct {
	port serialdev.Port

	mu      sync.Mutex
	history bytes.Buffer // bounded backlog for sessions that attach later
	subs    map[int]*subscriber
	nextID  int
	closed  bool
	err     error
}

// historyBytes is how much of the recent past a newly attached session can
// still see. Serial consoles are slow, so this is minutes of typical output.
const historyBytes = 256 * 1024

// subscriberBytes bounds one attached session. A session that stops reading
// loses the oldest bytes rather than stalling the monitor for everyone.
const subscriberBytes = 512 * 1024

func newMonitor(port serialdev.Port) *monitor {
	m := &monitor{port: port, subs: map[int]*subscriber{}}
	go m.pump()
	return m
}

// pump reads the device forever, recording and fanning out.
func (m *monitor) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := m.port.Read(buf)
		if n > 0 {
			m.mu.Lock()
			appendBounded(&m.history, buf[:n], historyBytes)
			for _, s := range m.subs {
				s.push(buf[:n])
			}
			m.mu.Unlock()
		}
		if err != nil {
			m.mu.Lock()
			m.closed, m.err = true, err
			for _, s := range m.subs {
				s.close()
			}
			m.mu.Unlock()
			return
		}
	}
}

// attach returns a reader over the recorded backlog followed by live output.
// withHistory replays what the device said before this session existed, which
// is the point of monitoring at all.
func (m *monitor) attach(withHistory bool) *subscriber {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := newSubscriber(m, m.nextID)
	m.nextID++
	if withHistory && m.history.Len() > 0 {
		s.push(m.history.Bytes())
	}
	if m.closed {
		s.close()
		return s
	}
	m.subs[s.id] = s
	return s
}

func (m *monitor) detach(id int) {
	m.mu.Lock()
	delete(m.subs, id)
	m.mu.Unlock()
}

// Close ends monitoring and releases the port. Any attached session ends too.
func (m *monitor) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	for _, s := range m.subs {
		s.close()
	}
	m.mu.Unlock()
	return m.port.Close()
}

// appendBounded keeps the newest max bytes.
func appendBounded(b *bytes.Buffer, p []byte, max int) {
	if len(p) >= max {
		b.Reset()
		b.Write(p[len(p)-max:])
		return
	}
	if b.Len()+len(p) > max {
		b.Next(b.Len() + len(p) - max)
	}
	b.Write(p)
}

// subscriber is one session's view of a monitored device: a bounded stream of
// what the device has said, readable with normal blocking semantics.
type subscriber struct {
	m  *monitor
	id int

	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newSubscriber(m *monitor, id int) *subscriber {
	s := &subscriber{m: m, id: id}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// push must be called with the monitor lock held.
func (s *subscriber) push(p []byte) {
	s.mu.Lock()
	if !s.closed {
		appendBounded(&s.buf, p, subscriberBytes)
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

func (s *subscriber) close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *subscriber) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.buf.Len() == 0 {
		if s.closed {
			return 0, io.EOF
		}
		s.cond.Wait()
	}
	return s.buf.Read(p)
}

// Write goes to the device itself; the monitor owns the port.
func (s *subscriber) Write(p []byte) (int, error) {
	if s.m == nil {
		return 0, errors.New("agent: monitor detached")
	}
	return s.m.port.Write(p)
}

// Close detaches this session and leaves the port open, which is the point:
// the next session attaches rather than reopening.
func (s *subscriber) Close() error {
	s.close()
	s.m.detach(s.id)
	return nil
}

func (s *subscriber) Drain() error { return s.m.port.Drain() }

func (s *subscriber) SetParams(baud *int, dtr, rts *bool) error {
	return s.m.port.SetParams(baud, dtr, rts)
}

// monitorPort is the serialdev.Port a monitored session is handed. It satisfies
// the same interface as a directly opened port, so nothing downstream needs to
// know whether it is talking to hardware or to a monitor.
var _ serialdev.Port = (*subscriber)(nil)
