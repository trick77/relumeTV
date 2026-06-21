package main

import "testing"

func TestFrameStats_ZeroWhenIdle(t *testing.T) {
	if fps := newFrameStats().FPS(); fps != 0 {
		t.Fatalf("FPS() on a fresh counter = %d, want 0", fps)
	}
}

func TestFrameStats_RateOverWindow(t *testing.T) {
	fs := newFrameStats()
	// All marks land within milliseconds, well inside the 2s window, so the rate is
	// deterministic: n frames / window seconds. 50 frames over a 2s window → 25 fps.
	for i := 0; i < 50; i++ {
		fs.Mark()
	}
	if fps := fs.FPS(); fps != 25 {
		t.Fatalf("FPS() = %d, want 25 (50 frames / 2s window)", fps)
	}
}
