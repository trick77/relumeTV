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

// SetState records the colour from a v1 light state map ({on,bri,xy}), the shape
// produced by both the REST forward and entertainment.ToHueV1State. Called on the
// hot per-frame path, so it does only a cheap parse + map write. The xy field can
// arrive as []float64 (from ToHueV1State) or []any (decoded from the TV's JSON).
func (c *liveColors) SetState(v1id string, state map[string]any) {
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
	c.mu.Lock()
	c.m[v1id] = lc
	c.mu.Unlock()
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
