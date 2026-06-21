package main

import (
	"sort"
	"testing"
	"time"
)

// fakeClock is a controllable time source for the freshness window.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLiveColors(window time.Duration, clk *fakeClock) *liveColors {
	lc := newLiveColors(window)
	lc.now = clk.now
	return lc
}

func sortedDriven(lc *liveColors) []string {
	ids := lc.DrivenV1IDs()
	sort.Strings(ids)
	return ids
}

// A light is driven right after it is seen, via either feed path.
func TestLiveColors_DrivenWithinWindow_BothPaths(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	lc := newTestLiveColors(2*time.Second, clk)

	lc.SetState("3", map[string]any{"on": true, "xy": []float64{0.4, 0.4}, "bri": 100}) // REST path
	lc.SetStates(map[string]map[string]any{                                             // DTLS path
		"5": {"on": true, "xy": []float64{0.1, 0.2}, "bri": 50},
	})

	if got := sortedDriven(lc); len(got) != 2 || got[0] != "3" || got[1] != "5" {
		t.Fatalf("driven = %v, want [3 5]", got)
	}
}

// The driven set empties once the window elapses with no new frames — no retention.
func TestLiveColors_EmptiesAfterWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	lc := newTestLiveColors(2*time.Second, clk)

	lc.SetStates(map[string]map[string]any{"7": {"on": true}})
	clk.add(3 * time.Second) // past the window, no further frames

	if got := lc.DrivenV1IDs(); len(got) != 0 {
		t.Fatalf("driven should be empty after the window, got %v", got)
	}
}

// A fresh frame inside the window keeps the light driven (window slides).
func TestLiveColors_WindowSlidesOnRefresh(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	lc := newTestLiveColors(2*time.Second, clk)

	lc.SetStates(map[string]map[string]any{"7": {"on": true}})
	clk.add(1 * time.Second)
	lc.SetStates(map[string]map[string]any{"7": {"on": true}}) // refresh
	clk.add(1500 * time.Millisecond)                           // 1.5s since refresh, still < 2s

	if got := lc.DrivenV1IDs(); len(got) != 1 || got[0] != "7" {
		t.Fatalf("driven = %v, want [7]", got)
	}
}

// The colour stays available for the swatch even after the light drops out of the
// driven window — colour is sticky, driven is windowed.
func TestLiveColors_ColourStickyAfterWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	lc := newTestLiveColors(2*time.Second, clk)

	lc.SetStates(map[string]map[string]any{"7": {"on": true, "xy": []float64{0.6, 0.3}, "bri": 222}})
	clk.add(5 * time.Second)
	_ = lc.DrivenV1IDs() // prunes the freshness entry

	snap := lc.Snapshot()
	col, ok := snap["7"]
	if !ok || col.X != 0.6 || col.Bri != 222 {
		t.Fatalf("colour should be retained after the window, got %+v ok=%v", col, ok)
	}
}
