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
	// lightReads counts GET /lights/{id} polls (the TV's high-frequency light-state
	// reads). Accumulated like the writes so the poll rate stays visible in the Hz
	// rollup instead of flooding the log one line per request. Deliberately not fed
	// into total_hz/per_light_hz — those stay the control-write telltale.
	lightReads uint64
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

// recordLightRead accumulates one GET /lights/{id} poll for the summary. It
// deliberately does NOT touch lightsTouched: reads must not skew per_light_hz,
// which stays the control-write rate.
func (a *activityTracker) recordLightRead(id string) {
	a.mu.Lock()
	a.lightReads++
	a.mu.Unlock()
}

// markActivity stamps the most-recent-activity time from a non-REST source — the
// entertainment DTLS stream. In entertainment mode the TV streams frames over
// DTLS instead of REST writes, so without this the idle-off monitor (which
// watches lastActivity) would treat an actively-streaming TV as idle and turn
// the lights off mid-stream. The stream stopping then correctly lets idle-off fire.
func (a *activityTracker) markActivity() {
	a.mu.Lock()
	a.lastWriteAt = time.Now()
	a.mu.Unlock()
}

// idleGapLogFloor is the smallest inter-write gap worth logging when gap tracing
// is on. It exists to calibrate the idle-off timeout: during a real viewing
// session the largest legitimate gap (static/dark/paused scenes) must stay well
// below the configured idle-off timeout (-idle-off-timeout-rest /
// -idle-off-timeout-entertainment).
const idleGapLogFloor = time.Second

// gapTrace gates the temporary inter-write gap log used to calibrate the
// idle-off timeout. It is a dedicated env var (not -debug) so a calibration run
// is not buried under per-request http rx/tx spam: set RELUME_TV_GAP_TRACE=1 and
// grep "ambilight write gap". Remove once the timeout default is settled.
var gapTrace = os.Getenv("RELUME_TV_GAP_TRACE") != ""

// recordWriteTime stamps the time of an Ambilight light-state write for the
// idle-off monitor (independent of Debug). With RELUME_TV_GAP_TRACE set it also logs
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
	lightReads        uint64
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
		lightReads:        a.lightReads,
		lights:            len(a.lightsTouched),
		lastWriteAt:       a.lastWriteAt,
	}
	a.lightWrites = 0
	a.groupActionWrites = 0
	a.lightReads = 0
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
	// TV/firmware that does not open the stream. It clears in two ways: markStreamUp
	// (a stream-up that races a just-firing watchdog — "stream up wins"), and lazy
	// recovery (tryRecoverFallback) — the next activation attempt after recoveryCooldown
	// has elapsed clears it so a transient DTLS failure no longer pins the TV to REST
	// until restart. recoveryCooldown ≫ fallbackTimeout bounds the flap to at most one
	// short re-attempt per cooldown.
	fallback        bool
	fallbackTimeout time.Duration
	watchdog        *time.Timer
	// fallbackAt is when fallback last latched; tryRecoverFallback gates recovery on
	// now()-fallbackAt ≥ recoveryCooldown. recoveryCooldown ≤ 0 disables recovery
	// (fallback stays sticky until the next stream-up or restart).
	fallbackAt       time.Time
	recoveryCooldown time.Duration
	// now is the clock seam (default time.Now) so the lazy-recovery cooldown is
	// testable without sleeping. Read only under mu.
	now func() time.Time
	// gen is the watchdog generation token. It is bumped on every arm/disarm/
	// stream-up; a fired watchdog only acts if its captured generation still
	// matches, so a stale callback (already firing when it was stopped or
	// superseded) cannot leave a healthy TV stuck in fallback (M1).
	gen uint64
	// restNoticed gates the one-shot "TV is driving via REST without ever opening
	// an entertainment stream" log (state C). It latches once such a write is seen
	// and resets on any stream-state transition, so a steady REST session logs
	// exactly once instead of per write.
	restNoticed bool
}

// defaultRecoveryCooldown is how long a latched REST fallback persists before the
// next activation attempt may recover it. Much larger than the DTLS fallback
// watchdog so a persistently-failing TV re-attempts at most once per cooldown.
const defaultRecoveryCooldown = 90 * time.Second

func newStreamState(fallbackTimeout time.Duration) *streamState {
	return &streamState{
		fallbackTimeout:  fallbackTimeout,
		recoveryCooldown: defaultRecoveryCooldown,
		now:              time.Now,
	}
}

// setFallbackTimeout overrides how long relume-tv waits for the TV's DTLS stream
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

// setRecoveryCooldown overrides how long a latched REST fallback must persist
// before the next activation attempt is allowed to recover it (see
// tryRecoverFallback). A value ≤ 0 disables lazy recovery (sticky fallback). A
// negative value is treated as 0 (disabled). Call before serving.
func (s *streamState) setRecoveryCooldown(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.mu.Lock()
	s.recoveryCooldown = d
	s.mu.Unlock()
}

// clockNow reads the current time via the injectable seam, defaulting to time.Now
// if unset — defensive against a future bare streamState literal (a nil seam would
// otherwise panic on the first watchdog fire). Call under mu.
func (s *streamState) clockNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// tryRecoverFallback clears a latched REST fallback if recovery is enabled
// (recoveryCooldown > 0) and the fallback has persisted at least recoveryCooldown.
// It is the lazy, synchronous counterpart to the watchdog: called from the
// activation handler when the TV re-requests a stream, so a transient DTLS failure
// no longer pins the TV to REST until restart. Returns true only when it actually
// cleared a fallback (so the caller can log the recovery). No timer and no
// generation token: during fallback there is no pending watchdog, so clearing the
// flag under the lock cannot race the M1 "stream-up wins" invariant.
func (s *streamState) tryRecoverFallback() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fallback || s.recoveryCooldown <= 0 {
		return false
	}
	if s.clockNow().Sub(s.fallbackAt) < s.recoveryCooldown {
		return false
	}
	s.fallback = false
	return true
}

// inFallback reports whether relume-tv has fallen back to REST-follow.
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

// noteRESTDriving reports whether the caller should emit the one-shot state-C
// log: the TV is driving via a per-light REST write while no DTLS stream is up,
// no stream activation is in flight (active), and relume-tv is not in fallback —
// i.e. the TV simply never opened an entertainment stream. Returns true exactly
// once per REST-driving episode; resetNotice (called on any stream-state
// transition) re-arms it for the next episode.
func (s *streamState) noteRESTDriving() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamUp || s.fallback || s.active || s.restNoticed {
		return false
	}
	s.restNoticed = true
	return true
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
	s.restNoticed = false
	s.mu.Unlock()
}

// snapshot returns the current activation and owner for bridgeGroup.
func (s *streamState) snapshot() (active bool, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.owner
}

// armWatchdog starts (or restarts) the fallback timer after relume-tv confirms a
// stream activation in entertainment mode. If the TV does not open its DTLS
// stream (markStreamUp) before it fires, relume-tv falls back to REST-follow. No-op
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
	was := s.streamUp
	s.streamUp = true
	if s.watchdog != nil {
		s.watchdog.Stop()
		s.watchdog = nil
	}
	s.fallback = false
	s.gen++
	s.restNoticed = false
	s.mu.Unlock()
	// Edge-triggered: log the path switch only on the actual REST→entertainment
	// transition, not on a redundant stream-up for an already-up stream.
	if !was {
		log.Info("control path → entertainment: TV DTLS stream up, streaming to the hue bridge pro")
	}
}

// markStreamDown records that the TV's DTLS stream closed, so a later re-activation
// can arm the watchdog again. Edge-triggered: logs the entertainment→REST switch
// only when a stream that was actually up has now closed.
func (s *streamState) markStreamDown(log *slog.Logger) {
	s.mu.Lock()
	was := s.streamUp
	s.streamUp = false
	s.restNoticed = false
	s.mu.Unlock()
	if was {
		log.Info("control path → REST-follow: TV DTLS stream closed")
	}
}

// watchdogFired is invoked when the fallback timer elapses. Under the lock it
// no-ops if the stream came up in the meantime (streamUp) or if it was stopped/
// superseded (its generation no longer matches the current one). Otherwise it
// flips relume-tv (stickily, until the next stream-up) back to REST-follow:
// subsequent activations get the generic ack and GET /groups/1 reports the stream
// inactive, so the TV resumes per-light PUTs.
func (s *streamState) watchdogFired(gen uint64, log *slog.Logger) {
	s.mu.Lock()
	if s.streamUp || gen != s.gen {
		s.mu.Unlock()
		return // stream came up, or this timer was stopped/superseded
	}
	s.fallback = true
	s.fallbackAt = s.clockNow()
	s.active = false
	s.owner = ""
	s.restNoticed = false
	timeout := s.fallbackTimeout
	s.mu.Unlock()
	log.Warn("control path → REST-follow (fallback): TV did NOT open the DTLS stream in time "+
		"(re-acking activation as inactive so the TV resumes per-light PUTs)",
		"timeout", timeout.String())
}
