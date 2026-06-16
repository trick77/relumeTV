package entertainment

import (
	"context"
	"fmt"
	"log/slog"
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

// ProClient is the subset of *bridgepro.Client the streamer needs (an interface so
// the state machine can be unit-tested without a real Pro).
type ProClient interface {
	Lights() ([]bridgepro.Light, error)
	EntertainmentServices() ([]bridgepro.EntertainmentService, error)
	EntertainmentConfigs() ([]bridgepro.EntertainmentConfig, error)
	CreateEntertainmentConfig(name string, members []bridgepro.ConfigMember) (string, error)
	GetEntertainmentConfig(id string) (*bridgepro.EntertainmentConfigFull, error)
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

	// port overrides the Pro DTLS port (default 2100); for tests.
	port int
	// dial is the DTLS dialer seam (default dialPro); for tests.
	dial func(host string, port int, identity string, psk []byte) (net.Conn, error)

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool

	st state
}

// state is the runtime stream state, guarded by its own mutex so Push (hot path,
// ~25 Hz from the receiver) never blocks on establishment.
type state struct {
	mu         sync.Mutex
	conn       net.Conn
	configID   string
	colorSpace uint8
	remap      map[uint16]uint8            // TV v1 light id → Pro channel id
	latest     map[uint8]huestream.Channel // Pro channel id → latest colour
	seq        uint8
	path       string // "dtls" | "rest"
}

// NewProStreamer builds a streamer. clientKey is the Pro DTLS PSK (already
// hex-decoded); an empty key means DTLS is impossible and the streamer stays on the
// REST fallback permanently (with a one-time warning).
func NewProStreamer(pro ProClient, host, appKey string, clientKey []byte, fallback FallbackSink, log *slog.Logger) *ProStreamer {
	return &ProStreamer{
		pro: pro, host: host, appKey: appKey, clientKey: clientKey,
		fallback: fallback, log: log, dial: dialPro,
	}
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
		s.log.Warn("pro entertainment unavailable: no Pro clientKey on this pairing — re-pair to enable DTLS; staying on REST forward")
		s.setPath("rest")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true
	s.log.Info("pro entertainment: TV stream started, establishing DTLS path", "tv", remote)
	go s.run(ctx)
}

// Stop tears down the Pro stream when the TV disconnects: best-effort StopStream so
// the Pro area never stays active, then closes the DTLS connection.
func (s *ProStreamer) Stop(remote string) {
	s.mu.Lock()
	cancel := s.cancel
	s.running = false
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.teardown()
	s.log.Info("pro entertainment: TV stream stopped, torn down Pro path", "tv", remote)
}

// Push routes one decoded TV frame. With DTLS up it updates the per-channel colours
// the send loop streams; otherwise it forwards each channel via the REST fallback.
func (s *ProStreamer) Push(_ string, f *huestream.Frame) {
	s.st.mu.Lock()
	if s.st.path == "dtls" && s.st.conn != nil {
		s.st.colorSpace = f.ColorSpace
		for _, ch := range f.Channels {
			if proCh, ok := s.st.remap[ch.ID]; ok {
				s.st.latest[proCh] = huestream.Channel{ID: uint16(proCh), A: ch.A, B: ch.B, C: ch.C}
			}
		}
		s.st.mu.Unlock()
		return
	}
	s.st.mu.Unlock()

	// REST fallback path.
	if s.fallback == nil {
		return
	}
	for _, ch := range f.Channels {
		s.fallback(strconv.Itoa(int(ch.ID)), ToHueV1State(f.ColorSpace, ch))
	}
}

// run establishes the DTLS path (retrying on a backoff while the TV stays
// connected) and runs the steady-rate send loop.
func (s *ProStreamer) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := s.establish(ctx); err != nil {
			s.setPath("rest")
			s.log.Warn("pro entertainment unavailable, falling back to REST forward", "err", err)
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
	s.log.Info("pro entertainment config ready", "id", configID, "name", configName, "reused", reused, "channels", channels)

	if err := s.pro.StartStream(configID); err != nil {
		// A reused config can be left active=true if relume restarted mid-stream
		// (the Pro keeps the area active, ownerless). Starting an already-active
		// configuration is rejected, so stop it and retry once — otherwise Phase C
		// would fall back to REST permanently on every subsequent run.
		s.log.Warn("pro entertainment start rejected, stopping a leftover-active config and retrying", "id", configID, "err", err)
		_ = s.pro.StopStream(configID)
		if err := s.pro.StartStream(configID); err != nil {
			return fmt.Errorf("start stream (after stop): %w", err)
		}
	}
	s.log.Info("pro entertainment stream started", "id", configID)

	port := s.port
	if port == 0 {
		port = 2100
	}
	s.log.Info("pro DTLS stream connecting", "host", fmt.Sprintf("%s:%d", s.host, port), "identity", s.appKey)
	conn, err := s.dial(s.host, port, s.appKey, s.clientKey)
	if err != nil {
		_ = s.pro.StopStream(configID)
		return fmt.Errorf("dtls dial: %w", err)
	}
	s.log.Info("pro DTLS stream connected")

	s.st.mu.Lock()
	s.st.conn = conn
	s.st.configID = configID
	s.st.remap = remap
	s.st.latest = map[uint8]huestream.Channel{}
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
				s.log.Info("pro entertainment stream", "frames_5s", sent-prev, "channels", ch, "seq", seq)
				prev = sent
			}
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
			if _, err := conn.Write(huestream.Encode(frame)); err != nil {
				s.log.Warn("pro DTLS send failed, dropping to REST fallback", "err", err)
				s.teardown()
				return
			}
			sent++
		}
	}
}

// buildFrameLocked builds the current HueStream v2 frame from the latest colours.
// Caller holds st.mu.
func (s *ProStreamer) buildFrameLocked() *huestream.Frame {
	if len(s.st.latest) == 0 {
		return nil
	}
	ids := make([]int, 0, len(s.st.latest))
	for id := range s.st.latest {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	channels := make([]huestream.Channel, 0, len(ids))
	for _, id := range ids {
		channels = append(channels, s.st.latest[uint8(id)])
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
	s.st.path = "rest"
	s.st.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	if configID != "" {
		if err := s.pro.StopStream(configID); err != nil {
			s.log.Warn("pro entertainment stop stream", "id", configID, "err", err)
		} else {
			s.log.Info("pro entertainment stream stopped", "id", configID)
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
		members = append(members, bridgepro.ConfigMember{ServiceRID: svc, X: position(len(members))})
		svcToV1[svc] = v1
	}
	if len(members) == 0 {
		return "", nil, false, 0, fmt.Errorf("no color-capable lights with an entertainment service")
	}

	// Reuse an existing relume config if present, else create one.
	configs, err := s.pro.EntertainmentConfigs()
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("entertainment configs: %w", err)
	}
	for _, c := range configs {
		if c.Metadata.Name == configName {
			id, reused = c.ID, true
			break
		}
	}
	if id == "" {
		id, err = s.pro.CreateEntertainmentConfig(configName, members)
		if err != nil {
			return "", nil, false, 0, fmt.Errorf("create config: %w", err)
		}
	}

	// Read back the bridge-assigned channels (ground truth — do not assume 0..N-1).
	full, err := s.pro.GetEntertainmentConfig(id)
	if err != nil {
		return "", nil, false, 0, fmt.Errorf("get config: %w", err)
	}
	remap = map[uint16]uint8{}
	for _, ch := range full.Channels {
		if len(ch.Members) == 0 {
			continue
		}
		if v1, ok := svcToV1[ch.Members[0].Service.RID]; ok {
			remap[v1] = uint8(ch.ChannelID)
		}
	}
	if len(remap) == 0 {
		return "", nil, false, 0, fmt.Errorf("no channels mapped to TV light ids")
	}
	return id, remap, reused, len(remap), nil
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
func dialPro(host string, port int, identity string, psk []byte) (net.Conn, error) {
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
	hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return conn, nil
}
