package bridge

import (
	"sync"
	"time"
)

// ControlledSet tracks which Bridge Pro lights the TV is *currently* driving for
// Ambilight, as a sliding time window: a light counts as controlled only if the
// TV has driven it within the window. Lights the TV stops driving (e.g. removed
// from the Ambilight configuration) fall out automatically after the window, so
// the set follows the current configuration instead of accumulating forever.
//
// It is in-memory only and deliberately not persisted: the window is short, so
// after a restart the set is simply empty until the TV drives lights again — and
// the restart/idle flash is a no-op while the set is empty (we never flash lights
// we have not captured). See flashColor.
type ControlledSet struct {
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
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

// Current returns the light UUIDs the TV has driven within the window and prunes
// any that have aged out. Empty when the TV has driven nothing recently — the
// flash then targets nothing.
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
	return out
}
