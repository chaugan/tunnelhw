package agent

import (
	"sync"
	"time"
)

// Entry is one activity-log line. Metadata only — device payloads are never
// logged (ARCHITECTURE.md §7).
type Entry struct {
	Time  time.Time `json:"time"`
	Kind  string    `json:"kind"` // info|open|close|control|denied|tunnel|error
	Text  string    `json:"text"`
	Seq   uint64    `json:"seq"`
}

// Activity is a bounded in-memory ring of entries.
type Activity struct {
	mu   sync.Mutex
	buf  []Entry
	next uint64
	max  int
}

func newActivity(max int) *Activity { return &Activity{max: max} }

// Add appends an entry, evicting the oldest beyond capacity.
func (a *Activity) Add(kind, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	a.buf = append(a.buf, Entry{Time: time.Now(), Kind: kind, Text: text, Seq: a.next})
	if len(a.buf) > a.max {
		a.buf = a.buf[len(a.buf)-a.max:]
	}
}

// Since returns entries with Seq > after, oldest first.
func (a *Activity) Since(after uint64) []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Entry
	for _, e := range a.buf {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}
