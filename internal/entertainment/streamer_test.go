package entertainment

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/huestream"
)

// fakeConn is a non-blocking net.Conn for the streamer tests: Write succeeds
// immediately (unlike net.Pipe, which is synchronous and would block sendLoop with
// no reader). closes counts how many times Close was called so a test can assert the
// orphaned DTLS conn was actually torn down.
type fakeConn struct {
	closes  atomic.Int32
	onClose func()
}

func (c *fakeConn) Read(b []byte) (int, error)  { return 0, io.EOF }
func (c *fakeConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *fakeConn) Close() error {
	c.closes.Add(1)
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}
func (c *fakeConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// stubPro is an in-memory ProClient for the streamer tests.
type stubPro struct {
	lights   []bridgepro.Light
	services []bridgepro.EntertainmentService
	configs  []bridgepro.EntertainmentConfig
	full     *bridgepro.EntertainmentConfigFull
	fullByID map[string]*bridgepro.EntertainmentConfigFull
	created  string

	mu             sync.Mutex
	started        []string
	stopped        []string
	deleted        []string
	createdN       int
	createdMembers []bridgepro.ConfigMember
	createErr      error
	getErr         error
	// startBlockedUntilStop simulates a leftover-active config: StartStream is
	// rejected until StopStream has been called once.
	startBlockedUntilStop bool
}

func (s *stubPro) Lights() ([]bridgepro.Light, error) { return s.lights, nil }
func (s *stubPro) EntertainmentServices() ([]bridgepro.EntertainmentService, error) {
	return s.services, nil
}
func (s *stubPro) EntertainmentConfigs() ([]bridgepro.EntertainmentConfig, error) {
	return s.configs, nil
}
func (s *stubPro) CreateEntertainmentConfig(name string, members []bridgepro.ConfigMember) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.mu.Lock()
	s.createdN++
	s.createdMembers = members
	s.mu.Unlock()
	return s.created, nil
}
func (s *stubPro) GetEntertainmentConfig(id string) (*bridgepro.EntertainmentConfigFull, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if f, ok := s.fullByID[id]; ok {
		return f, nil
	}
	return s.full, nil
}
func (s *stubPro) DeleteEntertainmentConfig(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubPro) StartStream(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startBlockedUntilStop {
		return fmt.Errorf("configuration is already streaming")
	}
	s.started = append(s.started, id)
	return nil
}
func (s *stubPro) StopStream(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startBlockedUntilStop = false
	s.stopped = append(s.stopped, id)
	return nil
}

// testConfigID is a realistic 36-char entertainment_configuration UUID so the v2
// frame's config id round-trips through Encode/Parse without null padding.
const testConfigID = "abcdefab-1234-1234-1234-0123456789ab"

func colorLight(uuid, dev string) bridgepro.Light {
	l := bridgepro.Light{ID: uuid}
	l.Metadata.Name = "Test " + uuid
	l.Owner.RID = dev
	l.Color = &bridgepro.LightColor{}
	return l
}

// oneLightPro wires a single color light (uuid-A on device dev-A) to entertainment
// service svc-A, which the bridge places on channel 5. translate.LightsV1 assigns
// the (only) color light v1 id "1", so the expected map is {1: 5}.
func oneLightPro() *stubPro {
	full := &bridgepro.EntertainmentConfigFull{ID: "cfg-1"}
	ch := bridgepro.EntChannel{ChannelID: 5}
	ch.Members = append(ch.Members, struct {
		Service struct {
			RID   string `json:"rid"`
			RType string `json:"rtype"`
		} `json:"service"`
		Index int `json:"index"`
	}{})
	ch.Members[0].Service.RID = "svc-A"
	full.Channels = []bridgepro.EntChannel{ch}

	svc := bridgepro.EntertainmentService{ID: "svc-A"}
	svc.Owner.RID = "dev-A"
	return &stubPro{
		lights:   []bridgepro.Light{colorLight("uuid-A", "dev-A")},
		services: []bridgepro.EntertainmentService{svc},
		created:  testConfigID,
		full:     full,
	}
}

// twoLightPro returns a Pro with two color lights (v1 ids 1→svc-A, 2→svc-B, by the
// slice order translate.LightsV1 assigns over). full is the read-back the stub
// returns for any GetEntertainmentConfig (build it to cover the services the test
// expects to end up in the config).
func twoLightPro(full *bridgepro.EntertainmentConfigFull) *stubPro {
	svcA := bridgepro.EntertainmentService{ID: "svc-A"}
	svcA.Owner.RID = "dev-A"
	svcB := bridgepro.EntertainmentService{ID: "svc-B"}
	svcB.Owner.RID = "dev-B"
	return &stubPro{
		lights:   []bridgepro.Light{colorLight("uuid-A", "dev-A"), colorLight("uuid-B", "dev-B")},
		services: []bridgepro.EntertainmentService{svcA, svcB},
		created:  testConfigID,
		full:     full,
	}
}

// TestProStreamer_ensureConfig_honorsRequestedSubset: when the TV declared a light
// subset, the created config contains only those members, not all color lights.
func TestProStreamer_ensureConfig_honorsRequestedSubset(t *testing.T) {
	// Given: two color lights, but the TV asked for only v1 id 1 (svc-A).
	pro := twoLightPro(configFull(testConfigID, 5, "svc-A"))
	s := quietStreamer(pro, nil)
	s.SetRequestedMembers([]uint16{1})

	// When
	_, remap, _, channels, err := s.ensureConfig()

	// Then
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if len(pro.createdMembers) != 1 || pro.createdMembers[0].ServiceRID != "svc-A" {
		t.Fatalf("createdMembers = %+v, want exactly [svc-A]", pro.createdMembers)
	}
	if channels != 1 {
		t.Fatalf("channels = %d, want 1 (only the requested subset)", channels)
	}
	if _, ok := remap[2]; ok {
		t.Fatalf("remap unexpectedly contains v1 id 2 (light outside the TV subset)")
	}
}

// TestProStreamer_ensureConfig_noSubsetDrivesAllColorLights: with no subset declared
// (cold start), ensureConfig keeps the legacy behaviour and drives every color light.
func TestProStreamer_ensureConfig_noSubsetDrivesAllColorLights(t *testing.T) {
	// Given: two color lights, no SetRequestedMembers call.
	pro := twoLightPro(configFull(testConfigID, 5, "svc-A", "svc-B"))

	// When
	_, _, _, channels, err := quietStreamer(pro, nil).ensureConfig()

	// Then
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if len(pro.createdMembers) != 2 {
		t.Fatalf("createdMembers = %+v, want both lights (no subset → fallback to all)", pro.createdMembers)
	}
	if channels != 2 {
		t.Fatalf("channels = %d, want 2", channels)
	}
}

// TestProStreamer_ensureConfig_recreatesAllLightsConfigOnSubsetShrink: a persisted
// config covering ALL lights must be recreated (not reused) once the TV narrows to a
// subset — otherwise the Pro would keep driving the lights outside the subset. This
// guards the reuse path, where the member-loop filter alone would not apply.
func TestProStreamer_ensureConfig_recreatesAllLightsConfigOnSubsetShrink(t *testing.T) {
	// Given: an existing relume config over both lights, but the TV now wants only 1.
	pro := twoLightPro(nil)
	pro.configs = []bridgepro.EntertainmentConfig{{ID: testConfigID}}
	pro.configs[0].Metadata.Name = configName
	pro.created = "cfg-new"
	pro.fullByID = map[string]*bridgepro.EntertainmentConfigFull{
		testConfigID: configFull(testConfigID, 5, "svc-A", "svc-B"), // stale: all lights
		"cfg-new":    configFull("cfg-new", 5, "svc-A"),             // recreated: subset
	}
	s := quietStreamer(pro, nil)
	s.SetRequestedMembers([]uint16{1})

	// When
	id, _, reused, channels, err := s.ensureConfig()

	// Then
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if reused {
		t.Fatalf("reused = true, want recreate (all-lights config does not cover the subset)")
	}
	if id != "cfg-new" || channels != 1 {
		t.Fatalf("id=%q channels=%d, want id=cfg-new channels=1", id, channels)
	}
	var deletedStale bool
	for _, d := range pro.deleted {
		if d == testConfigID {
			deletedStale = true
		}
	}
	if !deletedStale {
		t.Fatalf("stale all-lights config %s was not deleted (deleted=%v)", testConfigID, pro.deleted)
	}
}

func quietStreamer(pro ProClient, fallback FallbackSink) *ProStreamer {
	return NewProStreamer(pro, "127.0.0.1", "proapp", []byte("0123456789abcdef"), fallback,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestProStreamer_ensureConfig_remapFromGroundTruth(t *testing.T) {
	// Given
	s := quietStreamer(oneLightPro(), nil)

	// When
	id, remap, reused, channels, err := s.ensureConfig()

	// Then
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if id != testConfigID || reused || channels != 1 {
		t.Fatalf("id=%q reused=%v channels=%d", id, reused, channels)
	}
	if got := remap[1]; got != 5 {
		t.Fatalf("remap[1] = %d, want 5 (bridge-assigned channel id, not 0..N-1)", got)
	}
}

// configFull builds an entertainment_configuration read-back with one channel per
// given service rid (channel ids start at base), for the membership tests.
func configFull(id string, base int, svcRIDs ...string) *bridgepro.EntertainmentConfigFull {
	full := &bridgepro.EntertainmentConfigFull{ID: id}
	for i, rid := range svcRIDs {
		ch := bridgepro.EntChannel{ChannelID: base + i}
		ch.Members = append(ch.Members, struct {
			Service struct {
				RID   string `json:"rid"`
				RType string `json:"rtype"`
			} `json:"service"`
			Index int `json:"index"`
		}{})
		ch.Members[0].Service.RID = rid
		full.Channels = append(full.Channels, ch)
	}
	return full
}

func TestProStreamer_ensureConfig_reusesMatchingConfig(t *testing.T) {
	// Given: a relume config that already covers the current light set (svc-A).
	pro := oneLightPro()
	pro.configs = []bridgepro.EntertainmentConfig{{ID: testConfigID}}
	pro.configs[0].Metadata.Name = configName

	// When
	id, _, reused, _, err := quietStreamer(pro, nil).ensureConfig()

	// Then: reused as-is, nothing deleted or created.
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if id != testConfigID || !reused {
		t.Fatalf("id=%q reused=%v, want %q reused", id, reused, testConfigID)
	}
	if len(pro.deleted) != 0 || pro.createdN != 0 {
		t.Fatalf("expected no delete/create: deleted=%v created=%d", pro.deleted, pro.createdN)
	}
}

func TestProStreamer_ensureConfig_recreatesOnLightSetChange(t *testing.T) {
	// Given: an existing relume config that covers a now-gone light (svc-OLD), while
	// the current color light maps to svc-A — the set changed under the config.
	pro := oneLightPro()
	pro.configs = []bridgepro.EntertainmentConfig{{ID: "stale-1"}}
	pro.configs[0].Metadata.Name = configName
	pro.fullByID = map[string]*bridgepro.EntertainmentConfigFull{
		"stale-1": configFull("stale-1", 9, "svc-OLD"),
	}

	// When
	id, remap, reused, _, err := quietStreamer(pro, nil).ensureConfig()

	// Then: the stale config is stopped+deleted and a fresh one created.
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if reused || id != testConfigID {
		t.Fatalf("id=%q reused=%v, want fresh %q", id, reused, testConfigID)
	}
	if len(pro.deleted) != 1 || pro.deleted[0] != "stale-1" {
		t.Fatalf("expected stale-1 deleted, got %v", pro.deleted)
	}
	if pro.createdN != 1 {
		t.Fatalf("expected 1 create, got %d", pro.createdN)
	}
	if remap[1] != 5 {
		t.Fatalf("remap[1]=%d, want 5", remap[1])
	}
}

func TestProStreamer_ensureConfig_cachesAcrossCalls(t *testing.T) {
	// Given: a fresh streamer (no existing configs → first call creates).
	pro := oneLightPro()
	s := quietStreamer(pro, nil)

	// When: two ensureConfig calls
	if _, _, _, _, err := s.ensureConfig(); err != nil {
		t.Fatalf("first ensureConfig: %v", err)
	}
	if _, _, reused, _, err := s.ensureConfig(); err != nil {
		t.Fatalf("second ensureConfig: %v", err)
	} else if !reused {
		t.Fatalf("second call should reuse the cached config")
	}

	// Then: only one create — the second call took the in-memory fast path.
	if pro.createdN != 1 {
		t.Fatalf("expected exactly 1 create across two calls, got %d", pro.createdN)
	}
}

func TestProStreamer_ensureConfig_reusesPersistedIDAndSaves(t *testing.T) {
	// Given: a persisted id pointing at a config whose NAME is not `relume` (proves
	// the id-based match, not the name fallback). It covers the current light set.
	pro := oneLightPro()
	pro.configs = []bridgepro.EntertainmentConfig{{ID: testConfigID}}
	pro.configs[0].Metadata.Name = "someone-elses-name"

	var saved string
	s := quietStreamer(pro, nil)
	s.SetConfigStore(func() string { return testConfigID }, func(id string) { saved = id })

	// When
	id, _, reused, _, err := s.ensureConfig()

	// Then: reused via the persisted id and re-saved.
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if id != testConfigID || !reused {
		t.Fatalf("id=%q reused=%v, want reused %q", id, reused, testConfigID)
	}
	if saved != testConfigID {
		t.Fatalf("saved=%q, want %q", saved, testConfigID)
	}
	if pro.createdN != 0 {
		t.Fatalf("expected no create, got %d", pro.createdN)
	}
}

func TestProStreamer_ensureConfig_transientGetDoesNotDuplicate(t *testing.T) {
	// Given: a listed relume config (so it exists), but reading it back fails
	// transiently — recreating would mint a duplicate.
	pro := oneLightPro()
	pro.configs = []bridgepro.EntertainmentConfig{{ID: testConfigID}}
	pro.configs[0].Metadata.Name = configName
	pro.getErr = fmt.Errorf("temporary network blip")

	// When
	_, _, _, _, err := quietStreamer(pro, nil).ensureConfig()

	// Then: it fails (→ REST fallback + backoff re-list) instead of creating a duplicate.
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if pro.createdN != 0 || len(pro.deleted) != 0 {
		t.Fatalf("expected no create/delete on transient error: created=%d deleted=%v", pro.createdN, pro.deleted)
	}
}

func TestProStreamer_establishStopsLeftoverActiveConfig(t *testing.T) {
	// Given: a reused relume config left active=true (relume restarted mid-stream),
	// so the first StartStream is rejected.
	pro := oneLightPro()
	pro.configs = []bridgepro.EntertainmentConfig{{ID: testConfigID, Status: "active"}}
	pro.configs[0].Metadata.Name = configName
	pro.startBlockedUntilStop = true

	called := make(chan net.Conn, 1)
	s := quietStreamer(pro, nil)
	s.dial = func(context.Context, string, int, string, []byte) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2
		called <- c1
		return c1, nil
	}

	// When
	if err := s.establish(context.Background()); err != nil {
		t.Fatalf("establish: %v", err)
	}
	s.teardown()

	// Then: it stopped the leftover-active config and then started successfully.
	pro.mu.Lock()
	defer pro.mu.Unlock()
	if len(pro.stopped) == 0 || len(pro.started) == 0 {
		t.Fatalf("expected stop-then-start: stopped=%v started=%v", pro.stopped, pro.started)
	}
}

func TestProStreamer_pushFallbackBeforeDTLS(t *testing.T) {
	// Given: a streamer that has not established DTLS — Push must use the REST sink.
	var mu sync.Mutex
	got := map[string]map[string]any{}
	s := quietStreamer(oneLightPro(), func(v1id string, state map[string]any) {
		mu.Lock()
		got[v1id] = state
		mu.Unlock()
	})

	// When: a frame for v1 light 1 (xy colorspace)
	s.Push("tv", &huestream.Frame{
		ColorSpace: huestream.ColorSpaceXY,
		Channels:   []huestream.Channel{{ID: 1, A: 0x4000, B: 0x6000, C: 0x8000}},
	})

	// Then: forwarded via the fallback as a v1 light state
	mu.Lock()
	defer mu.Unlock()
	if _, ok := got["1"]; !ok {
		t.Fatalf("fallback not called for light 1: %v", got)
	}
	if got["1"]["on"] != true {
		t.Fatalf("fallback state = %v", got["1"])
	}
}

func TestProStreamer_noClientKeyStaysREST(t *testing.T) {
	// Given: no Pro clientKey → DTLS impossible
	s := NewProStreamer(oneLightPro(), "127.0.0.1", "proapp", nil, func(string, map[string]any) {},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// When
	s.Start("tv")
	defer s.Stop("tv")

	// Then: path stays REST and no establishment goroutine runs
	if s.Path() != "rest" {
		t.Fatalf("path = %q, want rest", s.Path())
	}
}

// TestProStreamer_reconnectStress hammers Start→Push→Stop in a tight loop (the fast
// TV-reconnect pattern) and proves PROBLEM 1: an old run must not write s.st.conn
// after Stop tore it down, orphaning a DTLS conn that nothing closes.
//
// The fake dial blocks on the run ctx and only then returns a LIVE conn — modelling
// a handshake that completes right as Stop fires, so establish writes s.st.conn late
// (the H1 window). Each iteration joins on done after Stop, so:
//   - with the join: Stop already waited; teardown ran after run exited → conn closed
//     → live==0; the extra join returns instantly.
//   - without the join: Stop returns early, our join then waits for the late
//     successful write + run exit, exposing the orphaned (never-closed) conn → live>0.
//
// live counts open fake conns deterministically (no -race needed: H1 is a logical
// leak, not a data race; the conn counter is the detector). -race additionally
// polices the new s.done field.
func TestProStreamer_reconnectStress(t *testing.T) {
	pro := oneLightPro()
	var live atomic.Int32 // open fake conns; must be 0 once each run has exited
	s := quietStreamer(pro, nil)
	s.dial = func(ctx context.Context, _ string, _ int, _ string, _ []byte) (net.Conn, error) {
		c := &fakeConn{onClose: func() { live.Add(-1) }}
		<-ctx.Done() // complete the "handshake" exactly when Stop cancels
		live.Add(1)
		return c, nil // a late SUCCESSFUL conn — establish writes it into s.st
	}

	for i := 0; i < 100; i++ {
		s.Start("tv")
		s.Push("tv", &huestream.Frame{
			ColorSpace: huestream.ColorSpaceXY,
			Channels:   []huestream.Channel{{ID: 1, A: 0x4000, B: 0x6000, C: 0x8000}},
		})
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		s.Stop("tv")
		if done != nil {
			<-done // synchronize on the run goroutine's actual exit
		}
		if n := live.Load(); n != 0 {
			t.Fatalf("iteration %d: %d conn(s) left open after run exited — orphaned by a late establish write (PROBLEM 1)", i, n)
		}
	}

	// After the final Stop joined run + tore down: idle, no live conn on s.st.
	s.st.mu.Lock()
	conn := s.st.conn
	path := s.st.path
	s.st.mu.Unlock()
	if conn != nil {
		t.Fatalf("s.st.conn = %v, want nil after final Stop", conn)
	}
	if path != "rest" {
		t.Fatalf("s.st.path = %q, want rest after final Stop", path)
	}
	if s.Path() != "rest" {
		t.Fatalf("Path() = %q, want rest", s.Path())
	}
}

// TestProStreamer_stopCancelsInFlightDial proves PROBLEM 2: a Stop during the DTLS
// handshake cancels the in-flight dial via the run ctx and returns promptly (it does
// not block on the 10s cap), and the run goroutine has fully exited (done closed).
func TestProStreamer_stopCancelsInFlightDial(t *testing.T) {
	pro := oneLightPro()
	dialing := make(chan struct{})
	s := quietStreamer(pro, nil)
	s.dial = func(ctx context.Context, _ string, _ int, _ string, _ []byte) (net.Conn, error) {
		close(dialing)
		<-ctx.Done() // block until the run ctx is cancelled by Stop
		return nil, ctx.Err()
	}

	s.Start("tv")
	<-dialing // ensure the dial is in flight

	// Capture done so we can assert the run goroutine fully exited after Stop.
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		t.Fatal("expected a live run goroutine (done != nil) while dialing")
	}

	stopReturned := make(chan struct{})
	go func() { s.Stop("tv"); close(stopReturned) }()

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung — in-flight dial was not cancelled by the run ctx")
	}

	select {
	case <-done:
	default:
		t.Fatal("run goroutine did not exit (done not closed) after Stop returned")
	}
}

// TestProStreamer_dtlsLoopback drives the full Phase C path against a real DTLS
// Receiver standing in for the Pro: Start → ensure config → start stream → dial DTLS
// → send loop. A pushed TV frame for v1 light 1 must arrive re-encoded as a v2 frame
// on the bridge-assigned channel 5.
func TestProStreamer_dtlsLoopback(t *testing.T) {
	const port = 32200
	const appKey = "proapp"
	clientKey := []byte("0123456789abcdef")

	frames := make(chan *huestream.Frame, 16)
	recv := &Receiver{
		bindIP: "127.0.0.1",
		Port:   port,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookup: func(id string) ([]byte, bool) {
			if id == appKey {
				return clientKey, true
			}
			return nil, false
		},
		OnFrame: func(_ string, f *huestream.Frame) { frames <- f },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = recv.Run(ctx) }()

	// Wait until the receiver accepts a DTLS-PSK client.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := dialPro(context.Background(), "127.0.0.1", port, appKey, clientKey)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver not ready: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	pro := oneLightPro()
	s := quietStreamer(pro, nil)
	s.port = port

	s.Start("tv")
	defer s.Stop("tv")

	// Keep pushing the frame until the send loop is established and a frame arrives.
	push := func() {
		s.Push("tv", &huestream.Frame{
			ColorSpace: huestream.ColorSpaceXY,
			Channels:   []huestream.Channel{{ID: 1, A: 0x4000, B: 0x6000, C: 0x8000}},
		})
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(6 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Major == 2 && len(f.Channels) == 1 && f.Channels[0].ID == 5 {
				if f.Channels[0].A != 0x4000 || f.Channels[0].B != 0x6000 || f.Channels[0].C != 0x8000 {
					t.Fatalf("colour not passed through: %+v", f.Channels[0])
				}
				if f.ConfigID != testConfigID {
					t.Fatalf("config id = %q", f.ConfigID)
				}
				return // success
			}
		case <-tick.C:
			push()
		case <-timeout:
			t.Fatal("timed out waiting for a v2 frame on channel 5")
		}
	}
}

// TestSmoothToward_convergesMonotonically proves the per-tick smoothing eases a hard
// jump toward the target without overshooting and reaches it exactly in finite steps —
// the core property that turns a verbatim scene cut into a fast fade.
func TestSmoothToward_convergesMonotonically(t *testing.T) {
	cur := huestream.Channel{ID: 3, A: 0, B: 0, C: 0}
	target := huestream.Channel{ID: 3, A: 0xFFFF, B: 0x8000, C: 0x4000}

	prev := cur
	reached := false
	for i := 0; i < 200; i++ {
		next := smoothToward(prev, target)
		// Never overshoot: each component stays within [prev, target].
		if next.A < prev.A || next.A > target.A ||
			next.B < prev.B || next.B > target.B ||
			next.C < prev.C || next.C > target.C {
			t.Fatalf("step %d overshot: prev=%v next=%v target=%v", i, prev, next, target)
		}
		if next == target {
			reached = true
			break
		}
		// Must make progress on the largest component while still far away.
		if next == prev {
			t.Fatalf("step %d stalled before reaching target: %v (target %v)", i, next, target)
		}
		prev = next
	}
	if !reached {
		t.Fatalf("did not reach target within 200 ticks, stuck at %v (target %v)", prev, target)
	}
	// First step toward 0xFFFF should land near alpha*0xFFFF, not jump verbatim.
	first := smoothToward(huestream.Channel{ID: 3}, target)
	if first.A > 0xC000 {
		t.Fatalf("first step too aggressive (verbatim-ish): A=%d", first.A)
	}
}

// TestSmoothToward_snapsWhenClose verifies a sub-threshold delta snaps to target
// rather than rounding to a stall (integer EMA would otherwise never converge).
func TestSmoothToward_snapsWhenClose(t *testing.T) {
	cur := huestream.Channel{ID: 1, A: 1000, B: 65535, C: 0}
	target := huestream.Channel{ID: 1, A: 1000 + snapColorDelta, B: 65535 - snapColorDelta, C: snapColorDelta}
	got := smoothToward(cur, target)
	if got != target {
		t.Fatalf("within snap threshold should snap to target: got %v want %v", got, target)
	}
}

// TestBuildFrameLocked_smoothsTowardLatest checks the send loop emits the smoothed
// current, snapping a never-seen channel to its first value (no fade up from black)
// and easing a subsequent jump.
func TestBuildFrameLocked_smoothsTowardLatest(t *testing.T) {
	s := &ProStreamer{}
	s.st.colorSpace = huestream.ColorSpaceXY
	s.st.configID = "cfg"
	s.st.latest = map[uint8]huestream.Channel{
		7: {ID: 7, A: 100, B: 200, C: 300},
	}

	// First frame: channel never seen → snap, emitted == latest.
	f := s.buildFrameLocked()
	if f == nil || len(f.Channels) != 1 {
		t.Fatalf("want 1 channel, got %v", f)
	}
	if f.Channels[0] != (huestream.Channel{ID: 7, A: 100, B: 200, C: 300}) {
		t.Fatalf("first frame should snap to latest, got %v", f.Channels[0])
	}

	// Hard jump on the target: next frame must ease, landing strictly between.
	s.st.latest[7] = huestream.Channel{ID: 7, A: 50000, B: 200, C: 300}
	f = s.buildFrameLocked()
	got := f.Channels[0].A
	if got <= 100 || got >= 50000 {
		t.Fatalf("second frame A should ease between 100 and 50000, got %d", got)
	}
}

// TestAccumSendJumps_smoothedStreamHasSmallerJumps proves the send-path stat the rig
// uses for verification: a verbatim hard cut yields a large col jump, but the smoothed
// stream's largest per-tick jump stays well below it.
func TestAccumSendJumps_smoothedStreamHasSmallerJumps(t *testing.T) {
	const inputJump = 50000 - 100 // the TV's verbatim A jump
	s := &ProStreamer{}
	s.st.colorSpace = huestream.ColorSpaceXY
	s.st.latest = map[uint8]huestream.Channel{7: {ID: 7, A: 100, B: 200, C: 65535}}
	first := s.buildFrameLocked() // snaps to start

	s.st.latest[7] = huestream.Channel{ID: 7, A: 50000, B: 200, C: 65535} // hard cut
	var briJump, colJump uint32
	prev := first
	for i := 0; i < 5; i++ { // a handful of 20 ms ticks following the cut
		cur := s.buildFrameLocked()
		accumSendJumps(prev, cur, &briJump, &colJump)
		prev = cur
	}
	if colJump == 0 {
		t.Fatal("expected some colour movement on the sent stream")
	}
	if colJump >= inputJump {
		t.Fatalf("smoothed stream jump %d should be below verbatim input jump %d", colJump, inputJump)
	}
}
