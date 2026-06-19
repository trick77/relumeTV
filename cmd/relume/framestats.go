package main

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// proStats bundles relume's outgoing-to-Pro counters so they thread through the
// provider constructors as one value instead of three parameters. writes and
// coalesces are rolling per-second rates (successful REST writes and frames
// dropped by the optimistic path); fwdErrs is a cumulative count of failed Pro
// writes since start — a rate would mostly read 0 and hide that errors happened.
type proStats struct {
	writes    *frameStats
	coalesces *frameStats
	fwdErrs   *atomic.Uint64
}

func newProStats() *proStats {
	return &proStats{writes: newFrameStats(), coalesces: newFrameStats(), fwdErrs: new(atomic.Uint64)}
}

// fpsWindow is the trailing window over which the live entertainment frame rate is
// measured. Wide enough to smooth the ~25 Hz TV stream into a steady number, short
// enough that the rate drops to 0 promptly once the TV stops streaming.
const fpsWindow = 2 * time.Second

// frameStats tracks the live entertainment-stream frame rate for the web UI. The
// DTLS receiver calls Mark once per decoded TV frame (via OnActivity); FPS reports
// the rate over the trailing fpsWindow. It is in-memory only and naturally decays
// to 0 when frames stop arriving, so the UI shows a live rate only while the TV is
// actually streaming over DTLS.
type frameStats struct {
	mu    sync.Mutex
	times []time.Time // arrival times within the trailing window (oldest first)
}

func newFrameStats() *frameStats { return &frameStats{} }

// Mark records a frame arrival at now and drops any timestamps older than the window.
func (f *frameStats) Mark() {
	now := time.Now()
	f.mu.Lock()
	f.times = append(f.times, now)
	f.trim(now)
	f.mu.Unlock()
}

// FPS returns the rounded frame rate over the trailing window, 0 when idle.
func (f *frameStats) FPS() int {
	now := time.Now()
	f.mu.Lock()
	f.trim(now)
	n := len(f.times)
	f.mu.Unlock()
	return int(math.Round(float64(n) / fpsWindow.Seconds()))
}

// trim drops timestamps older than the window. Caller holds the lock.
func (f *frameStats) trim(now time.Time) {
	cut := now.Add(-fpsWindow)
	i := 0
	for i < len(f.times) && f.times[i].Before(cut) {
		i++
	}
	f.times = f.times[i:]
}
