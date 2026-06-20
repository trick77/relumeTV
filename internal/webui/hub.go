// Package webui provides relume's optional, opt-in web UI: a guided setup
// assistant and a live status dashboard. It is started only when -ui-port is
// non-zero and never touches the TV- or Pro-facing control paths.
package webui

import "sync"

// Event is a single human-readable log line surfaced to the UI's live event tail.
type Event struct {
	Time  string `json:"time"`  // RFC3339, set by the caller
	Level string `json:"level"` // e.g. "INFO", "WARN"
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"` // pre-formatted logfmt, e.g. `failures=3 last_err="i/o timeout"`
}

// Frame is one message pushed over the SSE stream: either a fresh snapshot or a
// single new event.
type Frame struct {
	Kind     string    `json:"kind"` // "snapshot" | "event"
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Event    *Event    `json:"event,omitempty"`
}

// Hub holds the latest snapshot and a bounded tail of recent events, and fans
// both out to SSE subscribers. All methods are safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	ringSize int
	events   []Event
	snap     Snapshot
	subs     map[int]chan Frame
	nextID   int
}

// NewHub creates a hub whose event ring keeps at most ringSize recent events.
func NewHub(ringSize int) *Hub {
	if ringSize < 1 {
		ringSize = 1
	}
	return &Hub{ringSize: ringSize, subs: map[int]chan Frame{}}
}

// PublishEvent appends an event to the bounded ring and fans it out to subscribers.
func (h *Hub) PublishEvent(e Event) {
	h.mu.Lock()
	h.events = append(h.events, e)
	if len(h.events) > h.ringSize {
		h.events = h.events[len(h.events)-h.ringSize:]
	}
	h.fanout(Frame{Kind: "event", Event: &e})
	h.mu.Unlock()
}

// SetSnapshot stores the latest snapshot and fans it out to subscribers.
func (h *Hub) SetSnapshot(s Snapshot) {
	h.mu.Lock()
	h.snap = s
	h.fanout(Frame{Kind: "snapshot", Snapshot: &s})
	h.mu.Unlock()
}

// Snapshot returns the most recent snapshot (zero value until the first SetSnapshot).
func (h *Hub) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}

// hasSubscribers reports whether any SSE client is currently connected. The
// snapshot loop uses this to avoid polling state (and the Hue Bridge Pro) when no
// browser is watching.
func (h *Hub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// Events returns a copy of the buffered events, oldest first.
func (h *Hub) Events() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, len(h.events))
	copy(out, h.events)
	return out
}

// fanout must be called with h.mu held. Sends are non-blocking per subscriber:
// a full (slow) subscriber drops this frame rather than stalling the hub.
func (h *Hub) fanout(f Frame) {
	for _, ch := range h.subs {
		select {
		case ch <- f:
		default:
		}
	}
}

// Subscribe registers a subscriber and returns its frame channel plus a cancel
// func that removes and closes it.
func (h *Hub) Subscribe() (<-chan Frame, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	ch := make(chan Frame, 32)
	h.subs[id] = ch
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
	return ch, cancel
}
