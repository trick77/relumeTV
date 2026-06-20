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
// lastErrUnix is the Unix time of the most recent failed write (0 = never), so the
// UI can decay the error indicator once a fault is long resolved.
type proStats struct {
	writes      *frameStats
	coalesces   *frameStats
	fwdErrs     *atomic.Uint64
	lastErrUnix *atomic.Int64
}

func newProStats() *proStats {
	return &proStats{
		writes:      newFrameStats(),
		coalesces:   newFrameStats(),
		fwdErrs:     new(atomic.Uint64),
		lastErrUnix: new(atomic.Int64),
	}
}

// markForwardErr records one failed write to the Pro: it bumps the cumulative
// count and stamps the time, so the UI shows a "N err" warning that decays once
// writes have been succeeding again for a while.
func (s *proStats) markForwardErr() {
	s.fwdErrs.Add(1)
	s.lastErrUnix.Store(time.Now().Unix())
}

// LastForwardErr returns the time of the most recent failed Pro write, or the zero
// time if none has happened.
func (s *proStats) LastForwardErr() time.Time {
	u := s.lastErrUnix.Load()
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(u, 0)
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

// jitterStaleAfter is how long a jump window stays valid without a refresh. The
// receiver/streamer report once per 5s rollup, so a little over two windows lets a
// single missed rollup pass without blanking, yet the metric clears to "no value"
// within ~12s of the stream stopping.
const jitterStaleAfter = 12 * time.Second

// jitterStats holds the latest per-window max brightness jump on the incoming TV
// stream (input) and on relume's smoothed sent stream (sent). The gap between them
// is the jitter the DTLS-path easing removed. Only the brightness axis is kept — it
// is the visible flicker — and only the most recent 5s window, stamped so the metric
// reads as "no value" (UI longdash) once the stream stops refreshing it.
type jitterStats struct {
	inputBri  atomic.Uint32
	sentBri   atomic.Uint32
	updatedAt atomic.Int64 // unix nanos of the last sent-side update
}

func newJitterStats() *jitterStats { return &jitterStats{} }

// setInput records the latest incoming-stream brightness jump.
func (j *jitterStats) setInput(briJump uint32) { j.inputBri.Store(briJump) }

// setSent records the latest sent-stream brightness jump and stamps freshness. The
// sent side only fires while relume is streaming to the Pro over DTLS, so its
// timestamp is what gates the metric on/off.
func (j *jitterStats) setSent(briJump uint32) {
	j.sentBri.Store(briJump)
	j.updatedAt.Store(time.Now().UnixNano())
}

// Reduction returns the latest input and sent brightness jumps and whether they are
// fresh. ok is false (UI shows a longdash) when no sent-side window has landed within
// jitterStaleAfter — i.e. relume is not streaming to the Pro over DTLS.
func (j *jitterStats) Reduction() (inBri, sentBri int, ok bool) {
	u := j.updatedAt.Load()
	if u == 0 || time.Since(time.Unix(0, u)) > jitterStaleAfter {
		return 0, 0, false
	}
	return int(j.inputBri.Load()), int(j.sentBri.Load()), true
}
