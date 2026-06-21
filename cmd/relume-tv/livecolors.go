package main

import (
	"sync"
	"time"

	"github.com/trick77/relume-tv/internal/webui"
)

// liveColors records the most recent colour the TV pushed for each v1 light id,
// captured at the two points relume-tv actually sees the colour values flow TV→Pro:
// the REST/fallback forward (bridge.LightProvider.OnColor) and the DTLS passthrough
// (entertainment.ProStreamer.OnColor). The web UI reads it so each lamp swatch
// shows the live streamed colour instead of the Hue Bridge Pro's REST light state,
// which the DTLS passthrough never updates.
//
// It also tracks WHEN each light was last seen, so it can answer which lights the
// TV is driving RIGHT NOW: DrivenV1IDs returns only the lights touched within a
// short freshness window. While the TV streams (DTLS ~50 Hz, or REST writes) the
// window stays full; once the stream stops the set empties within the window — so
// the UI count, the per-light "driven" marking and the manual flash all reflect
// the live set, not a sticky "ever driven this run" set. The colour map itself is
// kept sticky (never pruned) so the swatch retains the last colour after a stop.
type liveColors struct {
	mu     sync.Mutex
	m      map[string]webui.LiveColor
	seen   map[string]time.Time
	window time.Duration
	now    func() time.Time // seam for tests; defaults to time.Now
}

// drivenLightWindow is how recently the TV must have streamed/written a light for
// it to count as currently driven. 2s comfortably spans DTLS frame gaps (~50 Hz)
// and active REST write cadence, yet empties quickly once the TV stops.
const drivenLightWindow = 2 * time.Second

func newLiveColors(window time.Duration) *liveColors {
	return &liveColors{
		m:      map[string]webui.LiveColor{},
		seen:   map[string]time.Time{},
		window: window,
		now:    time.Now,
	}
}

// SetState records the colour from one v1 light state map ({on,bri,xy}). Used by
// the REST forward, which forwards one light at a time.
func (c *liveColors) SetState(v1id string, state map[string]any) {
	lc := parseLiveColor(state)
	c.mu.Lock()
	c.m[v1id] = lc
	c.seen[v1id] = c.now()
	c.mu.Unlock()
}

// SetStates records a whole frame's colours under a single lock. Used by the DTLS
// passthrough, which decodes all channels of a frame at once on the hot ~50 Hz
// path — one lock per frame instead of per channel.
func (c *liveColors) SetStates(states map[string]map[string]any) {
	now := c.now()
	c.mu.Lock()
	for v1id, state := range states {
		c.m[v1id] = parseLiveColor(state)
		c.seen[v1id] = now
	}
	c.mu.Unlock()
}

// DrivenV1IDs returns the v1 light ids the TV is driving right now: those seen
// within the freshness window. Entries older than the window are pruned, so once
// the TV stops streaming the set empties. The returned ids carry no order.
func (c *liveColors) DrivenV1IDs() []string {
	cutoff := c.now().Add(-c.window)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.seen))
	for id, t := range c.seen {
		if t.Before(cutoff) {
			delete(c.seen, id)
			continue
		}
		out = append(out, id)
	}
	return out
}

// parseLiveColor turns a v1 light state map ({on,bri,xy}) — the shape produced by
// both the REST forward and entertainment.ToHueV1State — into a LiveColor. The xy
// field can arrive as []float64 (from ToHueV1State) or []any (decoded TV JSON).
func parseLiveColor(state map[string]any) webui.LiveColor {
	lc := webui.LiveColor{On: true}
	if on, ok := state["on"].(bool); ok {
		lc.On = on
	}
	switch b := state["bri"].(type) {
	case int:
		lc.Bri = b
	case float64:
		lc.Bri = int(b)
	}
	switch xy := state["xy"].(type) {
	case []float64:
		if len(xy) == 2 {
			lc.X, lc.Y = xy[0], xy[1]
		}
	case []any:
		if len(xy) == 2 {
			lc.X, _ = xy[0].(float64)
			lc.Y, _ = xy[1].(float64)
		}
	}
	return lc
}

// Snapshot returns a copy of the current per-v1-id colours for a UI snapshot.
func (c *liveColors) Snapshot() map[string]webui.LiveColor {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]webui.LiveColor, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}
