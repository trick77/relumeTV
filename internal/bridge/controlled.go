package bridge

import (
	"sync"
	"time"
)

// ControlledSet tracks which Hue Bridge Pro lights the TV is driving for Ambilight,
// as a sliding time window: a light is fresh if the TV drove it within the window.
//
// A light only ever leaves the set when a NEWER, still non-empty window no longer
// contains it — i.e. lights drop out only because the configuration changed to a
// different (non-empty) set. We must never transition from "had lights" to "no
// lights": if the whole window ages out (the TV paused or went idle), the most
// recent non-empty set is RETAINED rather than reported as empty. Genuinely empty
// is only the initial state, before the TV has ever driven a light.
//
// It is in-memory only and deliberately not persisted: after a restart the set is
// empty until the TV drives lights again, and the flash is a no-op while empty (we
// never flash lights we have not captured). See flashColor.
type ControlledSet struct {
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
	// last is the most recent non-empty windowed set, retained so the window
	// emptying never collapses the set to nothing.
	last []string
}

// NewControlledSet creates a tracker with the given sliding window.
func NewControlledSet(window time.Duration) *ControlledSet {
	return &ControlledSet{window: window, now: time.Now, seen: map[string]time.Time{}}
}

// Seen records that the TV drove the given light UUID just now, refreshing its
// membership in the window. Called on every per-light forward.
func (s *ControlledSet) Seen(uuid string) {
	if uuid == "" {
		return
	}
	s.mu.Lock()
	s.seen[uuid] = s.now()
	s.mu.Unlock()
}

// Current returns the light UUIDs the TV is driving, pruning any that aged out of
// the window. While the window is non-empty it is the live set (and is remembered
// as the last non-empty set). Once the window empties, the last non-empty set is
// retained — so a pause or idle never collapses the set to nothing. Empty only
// before the TV has ever driven a light.
func (s *ControlledSet) Current() []string {
	cutoff := s.now().Add(-s.window)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.seen))
	for uuid, t := range s.seen {
		if t.Before(cutoff) {
			delete(s.seen, uuid)
			continue
		}
		out = append(out, uuid)
	}
	if len(out) > 0 {
		s.last = out
		return out
	}
	// Window empty → retain the last non-empty set (never "had lights, now none").
	return append([]string(nil), s.last...)
}
