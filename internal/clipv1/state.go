package clipv1

import (
	"log/slog"
	"os"
	"sync"
	"time"
)

// activityTracker accumulates the high-frequency light-state writes Ambilight
// sends (REST control path) so they can be summarized periodically instead of
// logging every single request, and stamps the most-recent-write time the
// idle-off monitor watches. It owns a single mutex guarding all of its fields.
type activityTracker struct {
	mu            sync.Mutex
	lightWrites   uint64
	lightsTouched map[string]struct{}
	// groupActionWrites counts PUT /groups/{id}/action writes — a second control
	// path the TV could push Ambilight frames over. Tallied so the activity Hz
	// reading cannot be faked out by frames arriving on the group endpoint.
	groupActionWrites uint64
	// lastWriteAt is the time of the most recent Ambilight light-state write,
	// stamped in recordWriteTime (so it is independent of Debug, unlike the
	// counters above). The idle-off monitor reads it via lastActivity.
	lastWriteAt time.Time
	log         *slog.Logger
}

func newActivityTracker(log *slog.Logger) *activityTracker {
	return &activityTracker{lightsTouched: map[string]struct{}{}, log: log}
}

// recordLightWrite accumulates one Ambilight light-state write for the periodic
// summary.
func (a *activityTracker) recordLightWrite(id string) {
	a.mu.Lock()
	a.lightWrites++
	a.lightsTouched[id] = struct{}{}
	a.mu.Unlock()
}

// recordGroupActionWrite accumulates one group-action write for the summary.
func (a *activityTracker) recordGroupActionWrite() {
	a.mu.Lock()
	a.groupActionWrites++
	a.mu.Unlock()
}

// markActivity stamps the most-recent-activity time from a non-REST source — the
// entertainment DTLS stream. In entertainment mode the TV streams frames over
// DTLS instead of REST writes, so without this the idle-off monitor (which
// watches lastActivity) would treat an actively-streaming TV as idle and flash
// the lights off mid-stream. The stream stopping then correctly lets idle-off fire.
func (a *activityTracker) markActivity() {
	a.mu.Lock()
	a.lastWriteAt = time.Now()
	a.mu.Unlock()
}

// idleGapLogFloor is the smallest inter-write gap worth logging when gap tracing
// is on. It exists to calibrate the idle-off timeout: during a real viewing
// session the largest legitimate gap (static/dark/paused scenes) must stay well
// below the configured -idle-off-timeout.
const idleGapLogFloor = time.Second

// gapTrace gates the temporary inter-write gap log used to calibrate the
// idle-off timeout. It is a dedicated env var (not -debug) so a calibration run
// is not buried under per-request http rx/tx spam: set RELUME_GAP_TRACE=1 and
// grep "ambilight write gap". Remove once the timeout default is settled.
var gapTrace = os.Getenv("RELUME_GAP_TRACE") != ""

// recordWriteTime stamps the time of an Ambilight light-state write for the
// idle-off monitor (independent of Debug). With RELUME_GAP_TRACE set it also logs
// the gap since the previous write when it exceeds idleGapLogFloor, to calibrate
// the idle-off timeout against the TV's real maximum legitimate pause.
func (a *activityTracker) recordWriteTime() {
	now := time.Now()
	a.mu.Lock()
	prev := a.lastWriteAt
	a.lastWriteAt = now
	a.mu.Unlock()
	if gapTrace && !prev.IsZero() {
		if gap := now.Sub(prev); gap >= idleGapLogFloor {
			a.log.Info("ambilight write gap", "gap", gap.Round(time.Millisecond).String())
		}
	}
}

// lastActivity returns the time of the most recent Ambilight light-state write
// (zero if none yet). Used by the idle-off monitor.
func (a *activityTracker) lastActivity() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastWriteAt
}

// activitySnapshot is the data flushActivity needs from the tracker, extracted
// (and reset) in one locked operation.
type activitySnapshot struct {
	lightWrites       uint64
	groupActionWrites uint64
	lights            int
	lastWriteAt       time.Time
}

// snapshotAndReset returns the accumulated counts and the last-write time, then
// resets the counters — atomically under the single tracker lock so a write
// arriving mid-flush is never lost or double-counted.
func (a *activityTracker) snapshotAndReset() activitySnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := activitySnapshot{
		lightWrites:       a.lightWrites,
		groupActionWrites: a.groupActionWrites,
		lights:            len(a.lightsTouched),
		lastWriteAt:       a.lastWriteAt,
	}
	a.lightWrites = 0
	a.groupActionWrites = 0
	a.lightsTouched = map[string]struct{}{}
	return snap
}

// streamState owns the entertainment/DTLS stream state behind ONE mutex: the
// requested activation (active + owner), whether the TV's DTLS stream is up, the
// sticky REST fallback flag, and the fallback watchdog timer. Folding all of this
// behind a single lock makes the "stream came up wins over a racing watchdog"
// invariant hold deterministically (M1), rather than relying on the ordering of
// separate atomics.
type streamState struct {
	mu     sync.Mutex
	active bool
	owner  string
	// streamUp tracks whether a TV DTLS stream is currently connected, so a
	// re-activation while the stream is healthy does not arm a watchdog that would
	// then falsely fall back (no fresh OnStreamStart fires for an already-up stream).
	streamUp bool
	// fallback flips entertainment mode back to REST-follow when the TV confirmed
	// stream activation but never opened the DTLS stream within fallbackTimeout. It
	// is a safety net so entertainment mode never leaves the lights unfollowed on a
	// TV/firmware that does not open the stream. markStreamUp clears it under the
	// lock, but note: once fallback latches, confirmsEntertainment() reports false,
	// so the TV stays on REST and stops opening DTLS streams — markStreamUp then does
	// not fire again in normal operation and fallback effectively persists until
	// restart. The clear exists for the one reachable case: a stream-up that races a
	// just-firing watchdog ("stream up wins"), not a broad mid-session recovery.
	fallback        bool
	fallbackTimeout time.Duration
	watchdog        *time.Timer
	// gen is the watchdog generation token. It is bumped on every arm/disarm/
	// stream-up; a fired watchdog only acts if its captured generation still
	// matches, so a stale callback (already firing when it was stopped or
	// superseded) cannot leave a healthy TV stuck in fallback (M1).
	gen uint64
}

func newStreamState(fallbackTimeout time.Duration) *streamState {
	return &streamState{fallbackTimeout: fallbackTimeout}
}

// setFallbackTimeout overrides how long relume waits for the TV's DTLS stream
// after confirming activation before reverting to REST-follow. A non-positive
// value keeps the existing value. Call before serving.
func (s *streamState) setFallbackTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.fallbackTimeout = d
	s.mu.Unlock()
}

// inFallback reports whether relume has fallen back to REST-follow.
func (s *streamState) inFallback() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fallback
}

// isUp reports whether a TV DTLS stream is currently connected (read-only, for
// the web UI).
func (s *streamState) isUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamUp
}

// setActive records the activation (or deactivation) the TV requested so
// GET /groups/1 reflects it. owner is set on activate, cleared on deactivate.
func (s *streamState) setActive(active bool, owner string) {
	s.mu.Lock()
	s.active = active
	if active {
		s.owner = owner
	} else {
		s.owner = ""
	}
	s.mu.Unlock()
}

// snapshot returns the current activation and owner for bridgeGroup.
func (s *streamState) snapshot() (active bool, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.owner
}

// armWatchdog starts (or restarts) the fallback timer after relume confirms a
// stream activation in entertainment mode. If the TV does not open its DTLS
// stream (markStreamUp) before it fires, relume falls back to REST-follow. No-op
// when not in entertainment mode, once already fallen back, or while the stream
// is already up (no fresh stream-start would fire to disarm it).
func (s *streamState) armWatchdog(entertainment bool, log *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !entertainment || s.fallback || s.streamUp {
		return
	}
	if s.watchdog != nil {
		s.watchdog.Stop()
	}
	s.gen++
	g := s.gen
	log.Info("entertainment: activation confirmed, awaiting TV DTLS stream on :2100",
		"fallback_in", s.fallbackTimeout.String())
	s.watchdog = time.AfterFunc(s.fallbackTimeout, func() { s.watchdogFired(g, log) })
}

// disarmWatchdog cancels a pending fallback timer and supersedes any already-firing
// callback (via the generation bump).
func (s *streamState) disarmWatchdog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchdog != nil {
		s.watchdog.Stop()
		s.watchdog = nil
	}
	s.gen++
}

// markStreamUp records that the TV opened its DTLS entertainment stream. Under the
// lock it stops the watchdog, clears fallback and bumps the generation — so a
// watchdog callback that began firing just before this call is superseded and
// cannot stickily fall back a healthy TV (M1: "stream came up wins over a racing
// watchdog"). Clearing fallback matters for that race; once fallback has latched in
// steady state the TV stays on REST and this no longer fires (see streamState.fallback).
func (s *streamState) markStreamUp(log *slog.Logger) {
	s.mu.Lock()
	s.streamUp = true
	if s.watchdog != nil {
		s.watchdog.Stop()
		s.watchdog = nil
	}
	s.fallback = false
	s.gen++
	s.mu.Unlock()
	log.Info("entertainment: TV DTLS stream up — entertainment path active")
}

// markStreamDown records that the TV's DTLS stream closed, so a later re-activation
// can arm the watchdog again.
func (s *streamState) markStreamDown() {
	s.mu.Lock()
	s.streamUp = false
	s.mu.Unlock()
}

// watchdogFired is invoked when the fallback timer elapses. Under the lock it
// no-ops if the stream came up in the meantime (streamUp) or if it was stopped/
// superseded (its generation no longer matches the current one). Otherwise it
// flips relume (stickily, until the next stream-up) back to REST-follow:
// subsequent activations get the generic ack and GET /groups/1 reports the stream
// inactive, so the TV resumes per-light PUTs.
func (s *streamState) watchdogFired(gen uint64, log *slog.Logger) {
	s.mu.Lock()
	if s.streamUp || gen != s.gen {
		s.mu.Unlock()
		return // stream came up, or this timer was stopped/superseded
	}
	s.fallback = true
	s.active = false
	s.owner = ""
	timeout := s.fallbackTimeout
	s.mu.Unlock()
	log.Warn("entertainment: TV did NOT open the DTLS stream in time — FALLING BACK to REST-follow "+
		"(re-acking activation as inactive so the TV resumes per-light PUTs)",
		"timeout", timeout.String())
}

// pairingGate defers auto-accepting the TV's first pairing for acceptDelay
// (measured from the first attempt seen), behind a single mutex.
type pairingGate struct {
	mu            sync.Mutex
	firstPairSeen time.Time
	// acceptDelay defers auto-accepting the TV's first pairing (measured from the
	// first attempt); defaults to defaultPairAcceptDelay, overridable in tests.
	acceptDelay time.Duration
}

func newPairingGate(acceptDelay time.Duration) *pairingGate {
	return &pairingGate{acceptDelay: acceptDelay}
}

// shouldDefer reports whether the current pairing attempt should still be held off
// (returning the standard 101), recording the first-attempt timestamp on the way.
// It returns true until acceptDelay has elapsed since that first attempt.
func (p *pairingGate) shouldDefer() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.firstPairSeen.IsZero() {
		p.firstPairSeen = time.Now()
	}
	return time.Since(p.firstPairSeen) < p.acceptDelay
}

// pending reports whether a pairing attempt has been seen and is still inside the
// accept window — a read-only view for the web UI, with none of shouldDefer's
// first-attempt-timestamp side effect.
func (p *pairingGate) pending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.firstPairSeen.IsZero() {
		return false
	}
	return time.Since(p.firstPairSeen) < p.acceptDelay
}
