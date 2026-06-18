package main

import (
	"sync"

	"github.com/trick77/relume/internal/webui"
)

// liveColors records the most recent colour the TV pushed for each v1 light id,
// captured at the two points relume actually sees the colour values flow TV→Pro:
// the REST/fallback forward (bridge.LightProvider.OnColor) and the DTLS passthrough
// (entertainment.ProStreamer.OnColor). The web UI reads it so each lamp swatch
// shows the live streamed colour instead of the Bridge Pro's REST light state,
// which the DTLS passthrough never updates. Presence of a light here also means the
// TV is driving it — the only per-light signal the DTLS path surfaces to the UI.
//
// It is in-memory only and never expires: after a stream stops, a light keeps its
// last colour, consistent with the retained ControlledSet (we never flip a driven
// light back to "not driven" just because the TV paused). Unlike ControlledSet it
// only ever adds, so if the TV's entertainment area is reconfigured mid-session a
// dropped light stays "driven" until restart — acceptable, restart clears it.
type liveColors struct {
	mu sync.Mutex
	m  map[string]webui.LiveColor
}

func newLiveColors() *liveColors {
	return &liveColors{m: map[string]webui.LiveColor{}}
}

// SetState records the colour from one v1 light state map ({on,bri,xy}). Used by
// the REST forward, which forwards one light at a time.
func (c *liveColors) SetState(v1id string, state map[string]any) {
	lc := parseLiveColor(state)
	c.mu.Lock()
	c.m[v1id] = lc
	c.mu.Unlock()
}

// SetStates records a whole frame's colours under a single lock. Used by the DTLS
// passthrough, which decodes all channels of a frame at once on the hot ~50 Hz
// path — one lock per frame instead of per channel.
func (c *liveColors) SetStates(states map[string]map[string]any) {
	c.mu.Lock()
	for v1id, state := range states {
		c.m[v1id] = parseLiveColor(state)
	}
	c.mu.Unlock()
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
