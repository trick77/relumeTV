// Package webui provides relume-tv's web UI: a guided setup assistant and a live
// status dashboard. It is on by default (disabled with -headless) and never
// touches the TV- or Pro-facing control paths.
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

// subscriber is one SSE client's frame channel plus the lock that makes a send and a
// concurrent cancel-close mutually exclusive — so fanout (which runs WITHOUT h.mu held)
// can never send on a channel that cancel is closing.
type subscriber struct {
	ch     chan Frame
	mu     sync.Mutex
	closed bool
}

// Hub holds the latest snapshot and a bounded tail of recent events, and fans
// both out to SSE subscribers. All methods are safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	ringSize int
	events   []Event
	snap     Snapshot
	subs     map[int]*subscriber
	nextID   int
}

// NewHub creates a hub whose event ring keeps at most ringSize recent events.
func NewHub(ringSize int) *Hub {
	if ringSize < 1 {
		ringSize = 1
	}
	return &Hub{ringSize: ringSize, subs: map[int]*subscriber{}}
}

// PublishEvent appends an event to the bounded ring and fans it out to subscribers.
func (h *Hub) PublishEvent(e Event) {
	h.mu.Lock()
	h.events = append(h.events, e)
	if len(h.events) > h.ringSize {
		h.events = h.events[len(h.events)-h.ringSize:]
	}
	subs := h.subList()
	h.mu.Unlock()
	fanout(subs, Frame{Kind: "event", Event: &e})
}

// SetSnapshot stores the latest snapshot and fans it out to subscribers.
func (h *Hub) SetSnapshot(s Snapshot) {
	h.mu.Lock()
	h.snap = s
	subs := h.subList()
	h.mu.Unlock()
	fanout(subs, Frame{Kind: "snapshot", Snapshot: &s})
}

// subList copies the current subscribers under h.mu so the actual sends happen with
// h.mu released. The process-wide logger funnels every slog line into PublishEvent, so
// fanning out while holding h.mu would self-deadlock the moment any code on the send
// path logged. Caller holds h.mu.
func (h *Hub) subList() []*subscriber {
	subs := make([]*subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	return subs
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

// fanout delivers a frame to each subscriber without blocking the hub. The per-sub
// lock makes each send mutually exclusive with a concurrent cancel-close. A snapshot is
// coalescible state: if a slow subscriber's buffer is full, the oldest queued frame is
// dropped to make room so the freshest snapshot still lands (a briefly-stalled
// dashboard would otherwise stay on stale state). An event is an append log: if the
// buffer is full it is simply dropped.
func fanout(subs []*subscriber, f Frame) {
	for _, s := range subs {
		s.mu.Lock()
		if !s.closed {
			select {
			case s.ch <- f:
			default:
				if f.Snapshot != nil {
					select {
					case <-s.ch: // drop the oldest queued frame…
					default:
					}
					select {
					case s.ch <- f: // …and land the fresh snapshot
					default:
					}
				}
			}
		}
		s.mu.Unlock()
	}
}

// Subscribe registers a subscriber and returns its frame channel plus a cancel
// func that removes and closes it.
func (h *Hub) Subscribe() (<-chan Frame, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	sub := &subscriber{ch: make(chan Frame, 32)}
	h.subs[id] = sub
	cancel := func() {
		h.mu.Lock()
		_, ok := h.subs[id]
		delete(h.subs, id)
		h.mu.Unlock()
		if !ok {
			return
		}
		// Close under the sub's own lock so a concurrent fanout send cannot fire after
		// the close (it checks s.closed under the same lock).
		sub.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		sub.mu.Unlock()
	}
	return sub.ch, cancel
}
