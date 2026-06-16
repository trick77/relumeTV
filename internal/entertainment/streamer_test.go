package entertainment

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/huestream"
)

// stubPro is an in-memory ProClient for the streamer tests.
type stubPro struct {
	lights   []bridgepro.Light
	services []bridgepro.EntertainmentService
	configs  []bridgepro.EntertainmentConfig
	full     *bridgepro.EntertainmentConfigFull
	created  string

	mu        sync.Mutex
	started   []string
	stopped   []string
	createErr error
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
func (s *stubPro) CreateEntertainmentConfig(name string, _ []bridgepro.ConfigMember) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	return s.created, nil
}
func (s *stubPro) GetEntertainmentConfig(string) (*bridgepro.EntertainmentConfigFull, error) {
	return s.full, nil
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

func TestProStreamer_establishStopsLeftoverActiveConfig(t *testing.T) {
	// Given: a reused relume config left active=true (relume restarted mid-stream),
	// so the first StartStream is rejected.
	pro := oneLightPro()
	pro.configs = []bridgepro.EntertainmentConfig{{ID: testConfigID, Status: "active"}}
	pro.configs[0].Metadata.Name = configName
	pro.startBlockedUntilStop = true

	called := make(chan net.Conn, 1)
	s := quietStreamer(pro, nil)
	s.dial = func(string, int, string, []byte) (net.Conn, error) {
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
		c, err := dialPro("127.0.0.1", port, appKey, clientKey)
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
