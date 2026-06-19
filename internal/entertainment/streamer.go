package entertainment

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/huestream"
	"github.com/trick77/relume/internal/translate"
)

// configName is the metadata.name relume uses for its own entertainment
// configuration on the Pro, so it can find and reuse it across restarts.
const configName = "relume"

// sendInterval is the steady rate at which relume re-sends the latest frame to the
// Pro. A continuous send keeps the Pro from auto-stopping the area when the TV's
// content is momentarily static.
const sendInterval = 20 * time.Millisecond // 50 Hz

// smoothTau is the exponential-smoothing time constant on the DTLS send path. The TV
// sends hard scene cuts (verified: bri/colour jumps of thousands of 16-bit units within
// one ~40 ms frame, no black flashes or drops) which, forwarded verbatim, read as a
// flicker. Each per-channel colour eases toward the latest TV frame with this time
// constant so a cut reaches the lamps as a fast fade. Kept to ~one TV-frame interval so
// the lag stays within the budget the DTLS path buys (M4) — tune here, it is the single knob.
const smoothTau = 40 * time.Millisecond

// snapColorDelta is the per-component distance (of 65535) within which current snaps to
// target instead of easing. Two jobs: it terminates the geometric tail (with integer
// rounding a sub-1 step would round to 0 and never converge) and skips streaming
// imperceptible micro-steps once within ~0.4% of the target.
const snapColorDelta = 256

// smoothAlpha is the per-tick EMA weight derived from smoothTau and sendInterval:
// alpha = 1 - exp(-dt/tau). Computed once so smoothTau stays the only thing to tune.
var smoothAlpha = 1 - math.Exp(-float64(sendInterval)/float64(smoothTau))

// smoothComponent eases one 16-bit colour component from cur toward tgt by smoothAlpha,
// snapping to tgt once within snapColorDelta (see snapColorDelta).
func smoothComponent(cur, tgt uint16) uint16 {
	d := int(tgt) - int(cur)
	if d <= snapColorDelta && d >= -snapColorDelta {
		return tgt
	}
	return uint16(int(cur) + int(math.Round(smoothAlpha*float64(d))))
}

// smoothToward eases current toward target on all three colour components (A/B/C —
// x/y/bri in XY, r/g/b in RGB), preserving target's channel ID. See smoothTau for why.
func smoothToward(current, target huestream.Channel) huestream.Channel {
	return huestream.Channel{
		ID: target.ID,
		A:  smoothComponent(current.A, target.A),
		B:  smoothComponent(current.B, target.B),
		C:  smoothComponent(current.C, target.C),
	}
}

// ProClient is the subset of *bridgepro.Client the streamer needs (an interface so
// the state machine can be unit-tested without a real Pro).
type ProClient interface {
	Lights() ([]bridgepro.Light, error)
	EntertainmentServices() ([]bridgepro.EntertainmentService, error)
	EntertainmentConfigs() ([]bridgepro.EntertainmentConfig, error)
	CreateEntertainmentConfig(name string, members []bridgepro.ConfigMember) (string, error)
	GetEntertainmentConfig(id string) (*bridgepro.EntertainmentConfigFull, error)
	DeleteEntertainmentConfig(id string) error
	StartStream(id string) error
	StopStream(id string) error
}

// FallbackSink forwards one light's v1 state via the REST path (clipv1.ForwardLight)
// when the DTLS stream to the Pro is unavailable (Phase B behaviour).
type FallbackSink func(v1id string, state map[string]any)

// ProStreamer owns relume's own entertainment stream to the Bridge Pro. On a TV
// stream (OnStreamStart) it ensures+starts a relume entertainment_configuration,
// dials a DTLS-PSK client to the Pro and re-encodes the decoded TV frames as
// HueStream v2 at a steady rate. If anything fails it falls back to the REST sink
// (Phase B) so the lights still follow — DTLS and REST are mutually exclusive.
type ProStreamer struct {
	pro       ProClient
	host      string
	appKey    string
	clientKey []byte
	fallback  FallbackSink
	log       *slog.Logger

	// OnColor, if set, is called once per DTLS-passthrough frame with a map of v1
	// light id → v1 state ({on,bri,xy}), so the web UI can show the live streamed
	// colour (and mark the light driven). Batched per frame to take one lock on the
	// hot path. The REST fallback path is covered by the provider's OnColor via the
	// fallback sink. Wired by main.
	OnColor func(states map[string]map[string]any)

	// OnSend, if set, is called once per frame successfully written to the Pro over
	// DTLS (the 50 Hz sendLoop). Lets the web UI show the live relume→Pro send rate,
	// the upsampled counterpart to the TV→relume input rate. Wired by main.
	OnSend func()

	// port overrides the Pro DTLS port (default 2100); for tests.
	port int
	// dial is the DTLS dialer seam (default dialPro); for tests. The ctx is the run
	// ctx so a Stop cancels an in-flight handshake (not just the 10s cap).
	dial func(ctx context.Context, host string, port int, identity string, psk []byte) (net.Conn, error)

	// loadCfgID / saveCfgID persist the resolved relume config id across restarts
	// (Phase D). Optional; nil keeps the streamer purely in-memory. Wired in main.go
	// to config.LoadEntConfigID / SaveEntConfigID. saveCfgID("") clears a stale id.
	loadCfgID func() string
	saveCfgID func(string)

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	// done is closed by the run goroutine when it returns; Stop waits on it after
	// cancelling so Start/Stop are strictly serial (the old run has fully exited
	// before teardown). nil when no run goroutine is live (Start returned early).
	done chan struct{}

	st state
}

// SetConfigStore wires optional persistence of the resolved relume config id (Phase
// D), so the streamer reuses its entertainment_configuration across restarts instead
// of re-finding it. load returns the persisted id (empty if none); save persists it
// (an empty id clears it). Call before Start.
func (s *ProStreamer) SetConfigStore(load func() string, save func(string)) {
	s.loadCfgID = load
	s.saveCfgID = save
}

// state is the runtime stream state, guarded by its own mutex so Push (hot path,
// ~25 Hz from the receiver) never blocks on establishment.
type state struct {
	mu         sync.Mutex
	conn       net.Conn
	configID   string
	colorSpace uint8
	remap      map[uint16]uint8            // TV v1 light id → Pro channel id
	latest     map[uint8]huestream.Channel // Pro channel id → latest TV colour (smoothing target)
	current    map[uint8]huestream.Channel // Pro channel id → eased colour actually streamed
	seq        uint8
	path       string // "dtls" | "rest"
	// cachedConfigID is the relume config id resolved earlier this process, so repeat
	// establish calls (stream re-connects, backoff retries) skip the list+match
	// round-trips. Guarded by st.mu: cheap and still correct now that Stop joins the
	// run goroutine (only one run touches it at a time) — the guard also covers the
	// Path/Push readers on other goroutines.
	cachedConfigID string
	// requested is the TV's entertainment-group light subset (v1 ids) as set by
	// SetRequestedMembers from the clipv1 group create/update. nil means no subset is
	// known yet, so ensureConfig (and Push's REST fallback) fall back to all
	// color-capable lights — the defensive default so the lights never all go dark.
	requested map[uint16]bool
}

// NewProStreamer builds a streamer. clientKey is the Pro DTLS PSK (already
// hex-decoded); an empty key means DTLS is impossible and the streamer stays on the
// REST fallback permanently (with a one-time warning).
func NewProStreamer(pro ProClient, host, appKey string, clientKey []byte, fallback FallbackSink, log *slog.Logger) *ProStreamer {
	return &ProStreamer{
		pro: pro, host: host, appKey: appKey, clientKey: clientKey,
		fallback: fallback, log: log,
		dial: dialPro,
	}
}

// SetRequestedMembers records the TV's entertainment-group light subset (v1 ids),
// as parsed from the clipv1 group create/update body. ensureConfig then restricts
// the Pro entertainment_configuration to exactly these lights, and Push's REST
// fallback forwards only their channels — so lights the TV did not put in its
// Ambilight zone are never driven. An empty list is ignored so a later stream
// activation that carries no lights array cannot clear an already-known subset.
func (s *ProStreamer) SetRequestedMembers(v1ids []uint16) {
	if len(v1ids) == 0 {
		return
	}
	m := make(map[uint16]bool, len(v1ids))
	for _, id := range v1ids {
		m[id] = true
	}
	s.st.mu.Lock()
	s.st.requested = m
	s.st.mu.Unlock()
}

// requestedMembers returns the current TV light subset (nil if none known). The map
// is replaced wholesale by SetRequestedMembers and never mutated in place, so the
// returned reference is safe to read without holding st.mu.
func (s *ProStreamer) requestedMembers() map[uint16]bool {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	return s.st.requested
}

// Path reports the currently active forward path ("dtls", "rest" or "" when idle) —
// surfaced in the periodic activity rollup as forward_path.
func (s *ProStreamer) Path() string {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if s.st.path == "" {
		return "rest"
	}
	return s.st.path
}

// Start establishes the Pro stream for a TV connection. Establishment runs in a
// goroutine so frames arriving immediately are not blocked; until DTLS is up, Push
// routes to the REST fallback. Safe to call repeatedly (no-op while already active).
func (s *ProStreamer) Start(remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	if len(s.clientKey) == 0 {
		s.log.Warn("hue bridge pro entertainment unavailable: no clientKey on this pairing — re-pair to enable DTLS; staying on REST forward")
		s.setPath("rest")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.cancel = cancel
	s.done = done
	s.running = true
	s.log.Info("hue bridge pro entertainment: TV stream started, establishing DTLS path", "tv", remote)
	go s.run(ctx, done)
}

// Stop tears down the Pro stream when the TV disconnects: best-effort StopStream so
// the Pro area never stays active, then closes the DTLS connection. It cancels the
// run goroutine and WAITS for it to exit before tearing down, so Start/Stop are
// strictly serial — the old run can no longer race a new one over s.st.
//
// This seriality assumes the caller invokes Start and Stop sequentially from a
// single goroutine — which the entertainment receiver does (OnStreamStart/OnStreamStop
// fire from its one read loop). Concurrent Start/Stop calls are NOT supported: an
// old Stop's teardown could disassemble the s.st of a just-started run.
func (s *ProStreamer) Stop(remote string) {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.running = false
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Join the run goroutine before teardown so it cannot overwrite s.st (conn/path)
	// after Stop has torn it down. Do NOT hold s.mu here — run uses s.st.mu, but a
	// goroutine join must never block on the Stop mutex. done is nil when Start
	// returned early (no clientKey → no run goroutine).
	if done != nil {
		<-done
	}
	s.teardown()
	s.log.Info("hue bridge pro entertainment: TV stream stopped, torn down hue bridge pro path", "tv", remote)
}

// Push routes one decoded TV frame. With DTLS up it updates the per-channel colours
// the send loop streams; otherwise it forwards each channel via the REST fallback.
func (s *ProStreamer) Push(_ string, f *huestream.Frame) {
	s.st.mu.Lock()
	if s.st.path == "dtls" && s.st.conn != nil {
		// Colour passes through verbatim on the DTLS path: the per-frame color space
		// (XY or RGB lives in the HueStream header, not in the config) and the raw
		// A/B/C 16-bit values are forwarded unchanged, so the Pro receives exactly
		// what the TV sent. No XY↔RGB conversion happens here (that is only the REST
		// fallback's job, in ToHueV1State). Real-hardware colour accuracy is a
		// hardware-only check — see PLAN.md "Optional".
		s.st.colorSpace = f.ColorSpace
		for _, ch := range f.Channels {
			if proCh, ok := s.st.remap[ch.ID]; ok {
				s.st.latest[proCh] = huestream.Channel{ID: uint16(proCh), A: ch.A, B: ch.B, C: ch.C}
			}
		}
		s.st.mu.Unlock()
		// Surface the live per-light colour to the UI (outside the stream lock). This
		// is the only point the DTLS passthrough exposes per-light state, so it also
		// drives the "driven" marking. ToHueV1State converts the raw frame colour to
		// the {on,bri,xy} shape the UI/REST path uses; the whole frame is handed over
		// at once so the store takes a single lock.
		if s.OnColor != nil {
			states := make(map[string]map[string]any, len(f.Channels))
			for _, ch := range f.Channels {
				states[strconv.Itoa(int(ch.ID))] = ToHueV1State(f.ColorSpace, ch)
			}
			s.OnColor(states)
		}
		return
	}
	s.st.mu.Unlock()

	// REST fallback path. Unlike the DTLS path (which is filtered for free by remap,
	// built only over the config members), this iterates the raw frame channels, so
	// the TV subset is applied explicitly here too — a light outside the requested
	// Ambilight set is never forwarded. ch.ID is the TV v1 light id (same contract as
	// remap, see above).
	if s.fallback == nil {
		return
	}
	requested := s.requestedMembers()
	for _, ch := range f.Channels {
		if requested != nil && !requested[ch.ID] {
			continue
		}
		s.fallback(strconv.Itoa(int(ch.ID)), ToHueV1State(f.ColorSpace, ch))
	}
}

// run establishes the DTLS path (retrying on a backoff while the TV stays
// connected) and runs the steady-rate send loop.
func (s *ProStreamer) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for ctx.Err() == nil {
		if err := s.establish(ctx); err != nil {
			s.setPath("rest")
			s.log.Warn("hue bridge pro entertainment unavailable, falling back to REST forward", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		s.sendLoop(ctx) // returns on ctx cancel or send error
	}
}

// establish ensures the relume entertainment_configuration exists and is started,
// then dials the DTLS-PSK client and builds the TV→Pro channel map.
func (s *ProStreamer) establish(ctx context.Context) error {
	configID, remap, reused, channels, err := s.ensureConfig()
	if err != nil {
		return fmt.Errorf("ensure config: %w", err)
	}
	s.log.Info("hue bridge pro entertainment config ready", "id", configID, "name", configName, "reused", reused, "channels", channels)

	if err := s.pro.StartStream(configID); err != nil {
		// A reused config can be left active=true if relume restarted mid-stream
		// (the Pro keeps the area active, ownerless). Starting an already-active
		// configuration is rejected, so stop it and retry once — otherwise Phase C
		// would fall back to REST permanently on every subsequent run.
		s.log.Warn("hue bridge pro entertainment start rejected, stopping a leftover-active config and retrying", "id", configID, "err", err)
		_ = s.pro.StopStream(configID)
		if err := s.pro.StartStream(configID); err != nil {
			return fmt.Errorf("start stream (after stop): %w", err)
		}
	}
	s.log.Info("hue bridge pro entertainment stream started", "id", configID)

	port := s.port
	if port == 0 {
		port = 2100
	}
	s.log.Info("hue bridge pro DTLS stream connecting", "host", fmt.Sprintf("%s:%d", s.host, port), "identity", s.appKey)
	conn, err := s.dial(ctx, s.host, port, s.appKey, s.clientKey)
	if err != nil {
		_ = s.pro.StopStream(configID)
		return fmt.Errorf("dtls dial: %w", err)
	}
	s.log.Info("hue bridge pro DTLS stream connected")

	s.st.mu.Lock()
	s.st.conn = conn
	s.st.configID = configID
	s.st.remap = remap
	s.st.latest = map[uint8]huestream.Channel{}
	s.st.current = map[uint8]huestream.Channel{}
	s.st.path = "dtls"
	s.st.mu.Unlock()
	return nil
}

// sendLoop encodes and sends the latest frame at the steady rate until ctx is
// cancelled or a send fails (which drops back to establish/fallback via run).
func (s *ProStreamer) sendLoop(ctx context.Context) {
	t := time.NewTicker(sendInterval)
	defer t.Stop()
	rollup := time.NewTicker(5 * time.Second)
	defer rollup.Stop()
	var sent, prev uint64
	// Per-window jump stats on the *sent* (smoothed) stream, mirroring the receiver's
	// input-side stats (5c817bb). With smoothing on, these should sit well below the
	// receiver's bri_max_jump/col_max_jump — that gap is the proof the easing works.
	var lastSent *huestream.Frame
	var briJump, colJump uint32

	for {
		select {
		case <-ctx.Done():
			return
		case <-rollup.C:
			s.st.mu.Lock()
			ch := len(s.st.latest)
			seq := s.st.seq
			s.st.mu.Unlock()
			if sent != prev {
				s.log.Info("hue bridge pro entertainment stream", "frames_5s", sent-prev, "channels", ch, "seq", seq,
					"bri_max_jump", briJump, "col_max_jump", colJump)
				prev = sent
			}
			briJump, colJump = 0, 0
		case <-t.C:
			s.st.mu.Lock()
			conn := s.st.conn
			if conn == nil {
				s.st.mu.Unlock()
				return
			}
			frame := s.buildFrameLocked()
			s.st.mu.Unlock()
			if frame == nil {
				continue // nothing to send yet
			}
			accumSendJumps(lastSent, frame, &briJump, &colJump)
			lastSent = frame
			if _, err := conn.Write(huestream.Encode(frame)); err != nil {
				s.log.Warn("hue bridge pro DTLS send failed, dropping to REST fallback", "err", err)
				s.teardown()
				return
			}
			sent++
			if s.OnSend != nil {
				s.OnSend()
			}
		}
	}
}

// accumSendJumps raises *briJump/*colJump to the largest per-channel brightness and
// colour jump between two consecutive sent frames, matching the receiver's input-side
// measure so the two rollup lines are directly comparable. No-op on the first frame or
// a channel-count change (e.g. just after a reconnect).
func accumSendJumps(prev, cur *huestream.Frame, briJump, colJump *uint32) {
	if prev == nil || len(prev.Channels) != len(cur.Channels) {
		return
	}
	for i := range cur.Channels {
		c, p := cur.Channels[i], prev.Channels[i]
		if d := absDiff(brightness(cur.ColorSpace, c), brightness(prev.ColorSpace, p)); d > *briJump {
			*briJump = d
		}
		cj := absDiff(uint32(c.A), uint32(p.A)) + absDiff(uint32(c.B), uint32(p.B))
		if cur.ColorSpace != huestream.ColorSpaceXY {
			cj += absDiff(uint32(c.C), uint32(p.C))
		}
		if cj > *colJump {
			*colJump = cj
		}
	}
}

// buildFrameLocked builds the next HueStream v2 frame, easing each channel's streamed
// colour (current) toward the latest TV colour (target) so hard cuts reach the lamps as
// a fast fade rather than a verbatim jump (see smoothTau). A channel seen for the first
// time snaps to its value (no fade up from black). Caller holds st.mu.
func (s *ProStreamer) buildFrameLocked() *huestream.Frame {
	if len(s.st.latest) == 0 {
		return nil
	}
	if s.st.current == nil {
		s.st.current = make(map[uint8]huestream.Channel, len(s.st.latest))
	}
	ids := make([]int, 0, len(s.st.latest))
	for id := range s.st.latest {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	channels := make([]huestream.Channel, 0, len(ids))
	for _, id := range ids {
		target := s.st.latest[uint8(id)]
		next := target // first sight of a channel: snap, don't fade up from black
		if cur, ok := s.st.current[uint8(id)]; ok {
			next = smoothToward(cur, target)
		}
		s.st.current[uint8(id)] = next
		channels = append(channels, next)
	}
	s.st.seq++
	return &huestream.Frame{
		Major:      2,
		Minor:      0,
		Sequence:   s.st.seq,
		ColorSpace: s.st.colorSpace,
		ConfigID:   s.st.configID,
		Channels:   channels,
	}
}

// teardown stops the Pro stream and closes the DTLS connection, flipping back to
// the REST path. Idempotent.
func (s *ProStreamer) teardown() {
	s.st.mu.Lock()
	conn := s.st.conn
	configID := s.st.configID
	s.st.conn = nil
	s.st.configID = ""
	s.st.path = "rest"
	s.st.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	if configID != "" {
		if err := s.pro.StopStream(configID); err != nil {
			s.log.Warn("hue bridge pro entertainment stop stream", "id", configID, "err", err)
		} else {
			s.log.Info("hue bridge pro entertainment stream stopped", "id", configID)
		}
	}
}

func (s *ProStreamer) setPath(p string) {
	s.st.mu.Lock()
	s.st.path = p
	s.st.mu.Unlock()
}

// ensureConfig finds the relume entertainment_configuration (or creates one
// covering all color-capable lights), then reads it back to learn the
// bridge-assigned channel ids and build the TV-v1-id → Pro-channel-id map.
func (s *ProStreamer) ensureConfig() (id string, remap map[uint16]uint8, reused bool, channels int, err error) {
	lights, err := s.pro.Lights()
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("lights: %w", err)
	}
	services, err := s.pro.EntertainmentServices()
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("entertainment services: %w", err)
	}

	// device rid → entertainment service rid
	devToSvc := make(map[string]string, len(services))
	for _, svc := range services {
		devToSvc[svc.Owner.RID] = svc.ID
	}
	// Pro light UUID → TV v1 light id (the SAME assignment the clipv1 provider
	// advertised to the TV, via translate.LightsV1 over the color-capable lights).
	lm := translate.LightsV1(lights)
	uuidToV1 := make(map[string]uint16, len(lm.V1ToUUID))
	for v1str, uuid := range lm.V1ToUUID {
		if n, perr := strconv.Atoi(v1str); perr == nil {
			uuidToV1[uuid] = uint16(n)
		}
	}

	// Honor the TV's group membership: if the TV told relume which lights belong to
	// its Ambilight zone (via POST/PUT /groups with a lights array), restrict the
	// config to exactly that subset. nil → no subset known yet (cold start), so keep
	// the legacy "all color lights" behaviour so nothing is ever accidentally dark.
	requested := s.requestedMembers()

	// Build the members (color lights that own an entertainment service) and the
	// service-rid → TV-v1-id mapping used to interpret the bridge's channels.
	var members []bridgepro.ConfigMember
	svcToV1 := map[string]uint16{}
	for _, l := range lights {
		if !l.HasColor() {
			continue
		}
		svc, ok := devToSvc[l.Owner.RID]
		if !ok {
			continue
		}
		v1, ok := uuidToV1[l.ID]
		if !ok {
			continue
		}
		if requested != nil && !requested[v1] {
			continue // light is not in the TV's requested Ambilight subset
		}
		members = append(members, bridgepro.ConfigMember{ServiceRID: svc, X: position(len(members))})
		svcToV1[svc] = v1
	}
	if len(members) == 0 {
		return "", nil, false, 0, fmt.Errorf("no color-capable lights with an entertainment service")
	}
	// desired is the set of entertainment-service rids the config must cover — used to
	// detect a light-set change under a reused config (Phase D).
	desired := make(map[string]bool, len(members))
	for _, m := range members {
		desired[m.ServiceRID] = true
	}

	// Fast path: a config id resolved earlier this process — reuse it without listing,
	// as long as it still exists and covers the current light set.
	if cached := s.cachedID(); cached != "" {
		if full, gerr := s.pro.GetEntertainmentConfig(cached); gerr == nil && configCoversServices(full, desired) {
			remap = remapFromConfig(full, svcToV1)
			if len(remap) == 0 {
				return "", nil, false, 0, fmt.Errorf("no channels mapped to TV light ids")
			}
			return cached, remap, true, len(remap), nil
		}
		s.setCachedID("") // gone or stale — fall through to the authoritative path
	}

	// Slow path: list the Pro's configs (authoritative — prevents duplicate creates)
	// and pick the persisted id if still present, else a config named `relume`.
	configs, err := s.pro.EntertainmentConfigs()
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("entertainment configs: %w", err)
	}
	var persisted string
	if s.loadCfgID != nil {
		persisted = s.loadCfgID()
	}
	for _, c := range configs {
		if persisted != "" && c.ID == persisted {
			id = c.ID
			break
		}
	}
	if id == "" {
		for _, c := range configs {
			if c.Metadata.Name == configName {
				id = c.ID
				break
			}
		}
	}

	// Validate the candidate covers the current light set; if it changed (or the
	// config vanished), drop the stale config and recreate it.
	if id != "" {
		full, gerr := s.pro.GetEntertainmentConfig(id)
		switch {
		case gerr == nil && configCoversServices(full, desired):
			remap = remapFromConfig(full, svcToV1)
			if len(remap) == 0 {
				return "", nil, false, 0, fmt.Errorf("no channels mapped to TV light ids")
			}
			s.setCachedID(id)
			s.persistConfigID(id)
			return id, remap, true, len(remap), nil
		case gerr == nil:
			// The color-light set changed under the config — stop (in case it is
			// active) and delete it so it never lingers or hits the Pro's area limit.
			s.log.Info("hue bridge pro entertainment config stale (light set changed), recreating", "id", id)
			_ = s.pro.StopStream(id)
			if derr := s.pro.DeleteEntertainmentConfig(id); derr != nil {
				s.log.Warn("hue bridge pro entertainment delete stale config", "id", id, "err", derr)
			}
		default:
			// The candidate is in the authoritative list yet GetEntertainmentConfig
			// failed — a transient error, not a missing config. Do NOT recreate (that
			// would mint a duplicate `relume` config); fail so run() retries via the
			// REST fallback and re-lists on backoff.
			return "", nil, false, 0, fmt.Errorf("get candidate config %s: %w", id, gerr)
		}
		id = ""
	}

	// Create a fresh config and persist its id for reuse next stream/restart.
	id, err = s.pro.CreateEntertainmentConfig(configName, members)
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("create config: %w", err)
	}

	// Read back the bridge-assigned channels (ground truth — do not assume 0..N-1).
	full, err := s.pro.GetEntertainmentConfig(id)
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("get config: %w", err)
	}
	remap = remapFromConfig(full, svcToV1)
	if len(remap) == 0 {
		return "", nil, false, 0, fmt.Errorf("no channels mapped to TV light ids")
	}
	s.setCachedID(id)
	s.persistConfigID(id)
	return id, remap, false, len(remap), nil
}

// cachedID / setCachedID guard the in-process resolved config id under st.mu. Stop
// now joins the run goroutine, so only one run touches it at a time (the old
// old-run-vs-new-run race is gone); the guard is kept as it is cheap and still
// orders these writes against the Path/Push readers on other goroutines.
func (s *ProStreamer) cachedID() string {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	return s.st.cachedConfigID
}

func (s *ProStreamer) setCachedID(id string) {
	s.st.mu.Lock()
	s.st.cachedConfigID = id
	s.st.mu.Unlock()
}

// persistConfigID stores the resolved config id via the optional config store.
func (s *ProStreamer) persistConfigID(id string) {
	if s.saveCfgID != nil {
		s.saveCfgID(id)
	}
}

// configCoversServices reports whether full's channels reference exactly the desired
// entertainment-service rids — the membership check that detects a light-set change
// under a reused config. Compares sets (order-independent); empty-member channels
// are skipped, as the bridge can return placeholder channels.
func configCoversServices(full *bridgepro.EntertainmentConfigFull, desired map[string]bool) bool {
	have := make(map[string]bool, len(desired))
	for _, ch := range full.Channels {
		for _, m := range ch.Members {
			if m.Service.RID != "" {
				have[m.Service.RID] = true
			}
		}
	}
	if len(have) != len(desired) {
		return false
	}
	for rid := range desired {
		if !have[rid] {
			return false
		}
	}
	return true
}

// remapFromConfig builds the TV-v1-id → Pro-channel-id map from the bridge's
// read-back config (ground truth — the bridge assigns the channel ids).
func remapFromConfig(full *bridgepro.EntertainmentConfigFull, svcToV1 map[string]uint16) map[uint16]uint8 {
	remap := map[uint16]uint8{}
	for _, ch := range full.Channels {
		if len(ch.Members) == 0 {
			continue
		}
		if v1, ok := svcToV1[ch.Members[0].Service.RID]; ok {
			remap[v1] = uint8(ch.ChannelID)
		}
	}
	return remap
}

// position spreads members cosmetically along x, clamped to the Pro's valid
// position range [-1, 1]; pass-through streaming ignores the spatial layout, so the
// exact value does not matter — but an out-of-range value would be rejected by the
// POST, so a large light count must not push x past 1.0.
func position(i int) float64 {
	x := -1 + 0.2*float64(i)
	if x > 1 {
		return 1
	}
	return x
}

// dialPro opens a DTLS-PSK client connection to the Pro's entertainment endpoint,
// mirroring the receiver's server options (identity = the Pro application key, PSK
// = the Pro clientkey, TLS_PSK_WITH_AES_128_GCM_SHA256, no extended master secret).
func dialPro(ctx context.Context, host string, port int, identity string, psk []byte) (net.Conn, error) {
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("resolve %s:%d: %w", host, port, err)
	}
	conn, err := dtls.DialWithOptions("udp", raddr,
		dtls.WithPSK(func([]byte) ([]byte, error) { return psk, nil }),
		dtls.WithPSKIdentityHint([]byte(identity)),
		dtls.WithCipherSuites(dtls.TLS_PSK_WITH_AES_128_GCM_SHA256),
		dtls.WithExtendedMasterSecret(dtls.DisableExtendedMasterSecret),
	)
	if err != nil {
		return nil, err
	}
	// Honour BOTH the run ctx (so a Stop cancels an in-flight handshake) and a 10s cap.
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return conn, nil
}
