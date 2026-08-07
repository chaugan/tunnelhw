package relayapi

import (
	"sync"
	"testing"
)

type fakeResets struct {
	mu    sync.Mutex
	n     int
	epoch uint64
	known bool
}

func (f *fakeResets) ResetsFor(string, string) (int, uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n, f.epoch, f.known
}

func (f *fakeResets) set(n int) {
	f.mu.Lock()
	f.n = n
	f.mu.Unlock()
}

func (f *fakeResets) restartAgent(n int, epoch uint64) {
	f.mu.Lock()
	f.n, f.epoch = n, epoch
	f.mu.Unlock()
}

// A reset belongs to the read that discovers it and to no other. Repeating the
// flag on every later read would turn a one-off event into a permanent alarm,
// which is worse than staying silent: a consumer would learn to ignore it.
func TestResetIsReportedOnce(t *testing.T) {
	f := &fakeResets{n: 3, epoch: 7, known: true}
	s := &Session{resets: f, agentEpoch: 7}
	s.cond = sync.NewCond(&s.mu)
	s.resetsSeen = 3 // as recorded when the session opened

	if s.takeResetNews() {
		t.Fatal("a device that has not reset since open must not report one")
	}

	f.set(4)
	if !s.takeResetNews() {
		t.Fatal("want the reset reported on the first read that sees it")
	}
	if s.takeResetNews() {
		t.Fatal("want the reset reported only once")
	}

	// Two resets between reads still surface, once.
	f.set(6)
	if !s.takeResetNews() {
		t.Fatal("want a later reset reported")
	}
	if s.takeResetNews() {
		t.Fatal("want the later reset reported only once")
	}
}

// When the agent or device is no longer connected the count is unavailable.
// That is what EOF is for; inventing a reset here would be a fabricated
// diagnosis of hardware the relay can no longer see.
func TestUnknownDeviceReportsNoReset(t *testing.T) {
	s := &Session{resets: &fakeResets{n: 99, epoch: 1, known: false}, agentEpoch: 1}
	s.cond = sync.NewCond(&s.mu)
	if s.takeResetNews() {
		t.Fatal("an unreachable device must not be reported as reset")
	}

	var none *Session = &Session{}
	none.cond = sync.NewCond(&none.mu)
	if none.takeResetNews() {
		t.Fatal("a session with no reset source must not report one")
	}
}

// An agent that restarts begins counting again from zero. A session left over
// from the previous connection must not read the new agent's first reset as
// its own, nor have a high stale count hide a real one: counts from two
// different processes are not comparable at all.
func TestResetsAcrossAgentRestartAreNotCompared(t *testing.T) {
	f := &fakeResets{n: 5, epoch: 1, known: true}
	s := &Session{resets: f, agentEpoch: 1}
	s.cond = sync.NewCond(&s.mu)
	s.resetsSeen = 5

	// The agent restarts: fresh epoch, counter back to zero, then one reset.
	f.restartAgent(1, 2)
	if s.takeResetNews() {
		t.Fatal("a count from a new agent process must not be compared against an old one")
	}
	if s.resetsSeen != 5 {
		t.Fatalf("the stale baseline must be left alone, got %d", s.resetsSeen)
	}
}
