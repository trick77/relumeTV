package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/trick77/relumetv/internal/bridgepro"
	"github.com/trick77/relumetv/internal/config"
)

func TestParseServeOptions_discoveryDiagnostics(t *testing.T) {
	// When
	opts, err := parseServeOptions([]string{
		"-config", "test.json",
		"-http-port", "8080",
		"-advertise-ip", "192.0.2.10",
		"-debug",
		"-tv-ip", "192.0.2.30",
		"-discovery-burst-duration", "90s",
		"-discovery-burst-interval", "1s",
	})

	// Then
	if err != nil {
		t.Fatalf("parseServeOptions: %v", err)
	}
	if opts.configPath != "test.json" {
		t.Errorf("configPath = %q", opts.configPath)
	}
	if opts.httpPort != 8080 {
		t.Errorf("httpPort = %d", opts.httpPort)
	}
	if opts.advertiseIP != "192.0.2.10" {
		t.Errorf("advertiseIP = %q", opts.advertiseIP)
	}
	if !opts.debug {
		t.Fatal("debug = false")
	}
	if opts.tvIP != "192.0.2.30" {
		t.Errorf("tvIP = %q", opts.tvIP)
	}
	if opts.discoveryBurstDuration != 90*time.Second {
		t.Errorf("discoveryBurstDuration = %s", opts.discoveryBurstDuration)
	}
	if opts.discoveryBurstInterval != time.Second {
		t.Errorf("discoveryBurstInterval = %s", opts.discoveryBurstInterval)
	}
	if opts.disableSSDP {
		t.Fatal("disableSSDP = true (not requested)")
	}
}

func TestParseServeOptions_disableSSDP(t *testing.T) {
	// When
	opts, err := parseServeOptions([]string{"-disable-ssdp"})

	// Then
	if err != nil {
		t.Fatalf("parseServeOptions: %v", err)
	}
	if !opts.disableSSDP {
		t.Fatal("disableSSDP = false")
	}
}

func TestParseServeOptions_bridgeProAutoPairFlags(t *testing.T) {
	// When
	opts, err := parseServeOptions([]string{"-bridge-ip", "192.0.2.50", "-skip-tls-verify"})

	// Then
	if err != nil {
		t.Fatalf("parseServeOptions: %v", err)
	}
	if opts.bridgeIP != "192.0.2.50" {
		t.Errorf("bridgeIP = %q", opts.bridgeIP)
	}
	if !opts.skipTLS {
		t.Fatal("skipTLS = false")
	}
}

// fakeMembership implements zoneMembership: uuids resolves a UUID to its v1 id (a
// missing entry means "unknown"), zone is the set of v1 ids currently in the zone, and
// allowAll mirrors AllowsMember's nil-subset fallback (every light allowed).
type fakeMembership struct {
	uuids    map[string]string
	zone     map[uint16]bool
	allowAll bool
}

func (f fakeMembership) V1ForUUID(uuid string) (string, bool) {
	v1, ok := f.uuids[uuid]
	return v1, ok
}

func (f fakeMembership) AllowsMember(v1id uint16) bool {
	if f.allowAll {
		return true
	}
	return f.zone[v1id]
}

func TestInZoneUUIDs_dropsOffZoneKeepsInZoneAndUnknown(t *testing.T) {
	m := fakeMembership{
		uuids: map[string]string{"uuid-3": "3", "uuid-9": "9"}, // uuid-x is unknown
		zone:  map[uint16]bool{3: true},                        // only light 3 in zone
	}
	got := inZoneUUIDs(m, []string{"uuid-3", "uuid-9", "uuid-x"})

	// In-zone kept, off-zone dropped, unresolved kept (defensive — never flash nothing).
	want := map[string]bool{"uuid-3": true, "uuid-x": true}
	if len(got) != len(want) {
		t.Fatalf("inZoneUUIDs = %v, want keys %v", got, want)
	}
	for _, u := range got {
		if !want[u] {
			t.Fatalf("inZoneUUIDs kept %q, not wanted; got %v", u, got)
		}
	}
}

func TestInZoneUUIDs_noZonePassesAllThrough(t *testing.T) {
	m := fakeMembership{
		uuids:    map[string]string{"uuid-3": "3", "uuid-9": "9"},
		allowAll: true, // no subset declared → AllowsMember true for all
	}
	got := inZoneUUIDs(m, []string{"uuid-3", "uuid-9"})
	if len(got) != 2 {
		t.Fatalf("inZoneUUIDs with no zone = %v, want both kept", got)
	}
}

func TestIdleShouldFire(t *testing.T) {
	const timeout = 30 * time.Second
	base := time.Unix(1_700_000_000, 0)
	active := base // a real, non-zero activity time
	cases := []struct {
		name     string
		now      time.Time
		lastSeen time.Time
		fired    bool
		want     bool
	}{
		{"never active → never fires", base.Add(time.Hour), time.Time{}, false, false},
		{"active but within timeout", active.Add(29 * time.Second), active, false, false},
		{"active and idle past timeout", active.Add(31 * time.Second), active, false, true},
		{"exactly at timeout", active.Add(timeout), active, false, true},
		{"already fired this transition", active.Add(time.Hour), active, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := idleShouldFire(tc.now, tc.lastSeen, tc.fired, timeout); got != tc.want {
				t.Fatalf("idleShouldFire = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconnectProConfig_preservesCredentialsAndRefreshesHostCert(t *testing.T) {
	// Given a paired Pro and a new IP + cert discovered on reconnect
	old := &config.BridgePro{Host: "192.0.2.1", AppKey: "app", ClientKey: "CK", CertSHA256: "oldfp", SkipTLSVerify: false, DiscoveryID: "abc123"}

	// When
	got := reconnectProConfig(old, "192.0.2.2", "newfp", false)

	// Then: credentials survive (no re-pairing), host + cert are refreshed
	if got.AppKey != "app" || got.ClientKey != "CK" {
		t.Fatalf("credentials not preserved: %+v", got)
	}
	if got.Host != "192.0.2.2" || got.CertSHA256 != "newfp" {
		t.Errorf("host/cert not refreshed: %+v", got)
	}
	if got.SkipTLSVerify {
		t.Errorf("SkipTLSVerify = true, expected false")
	}
	// DiscoveryID identifies the same bridge across the reconnect → carried forward.
	if got.DiscoveryID != "abc123" {
		t.Errorf("DiscoveryID not carried: %q", got.DiscoveryID)
	}
}

func TestReconnectProConfig_skipTLSIsSticky(t *testing.T) {
	// Given a Pro that was paired with TLS verification skipped
	old := &config.BridgePro{Host: "h", AppKey: "a", ClientKey: "c", SkipTLSVerify: true}

	// When reconnecting without the global skip flag
	got := reconnectProConfig(old, "h2", "", false)

	// Then the prior skip setting is retained
	if !got.SkipTLSVerify {
		t.Fatal("SkipTLSVerify should remain true from the old config")
	}
}

func TestSleepCtx_returnsFalseWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx returned true for a cancelled context")
	}
}

func TestSleepCtx_returnsTrueAfterDelay(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("sleepCtx returned false after a normal delay")
	}
}

// testWatcher builds a proWatcher backed by a real (temp-path) config holding the
// given Pro, with the network seams replaced by the supplied fakes. Seams left nil
// are wired to harmless defaults so a test only overrides what it asserts on.
func testWatcher(t *testing.T, pro *config.BridgePro,
	healthCheck func(*config.BridgePro) error,
	discover func() ([]bridgepro.DiscoveredBridge, error),
	fetchFingerprint func(string) (string, error),
	applyProvider func(*config.BridgePro),
) (*proWatcher, *config.Config) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "relumetv.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.SetPro(pro); err != nil {
		t.Fatalf("SetPro: %v", err)
	}
	if discover == nil {
		discover = func() ([]bridgepro.DiscoveredBridge, error) { return nil, nil }
	}
	if fetchFingerprint == nil {
		fetchFingerprint = func(string) (string, error) { return "fp", nil }
	}
	if applyProvider == nil {
		applyProvider = func(*config.BridgePro) {}
	}
	w := &proWatcher{
		cfg:              cfg,
		bridgeIP:         "",
		skipTLS:          true, // skip cert fetch in tests unless a test overrides
		log:              slog.New(slog.NewTextHandler(testWriter{t}, nil)),
		healthCheck:      healthCheck,
		discover:         discover,
		fetchFingerprint: fetchFingerprint,
		applyProvider:    applyProvider,
	}
	return w, cfg
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Logf("%s", p); return len(p), nil }

func TestProWatcherTick_healthyDoesNotReconnect(t *testing.T) {
	discoverCalled := false
	w, cfg := testWatcher(t,
		&config.BridgePro{Host: "192.0.2.1", AppKey: "a"},
		func(*config.BridgePro) error { return nil }, // healthy
		func() ([]bridgepro.DiscoveredBridge, error) { discoverCalled = true; return nil, nil },
		nil, nil,
	)

	if w.tick() {
		t.Fatal("tick reported a reconnect on a healthy Pro")
	}
	if discoverCalled {
		t.Fatal("discover called for a healthy Pro")
	}
	if cfg.GetPro().Host != "192.0.2.1" {
		t.Errorf("Pro host changed: %q", cfg.GetPro().Host)
	}
}

func TestProWatcherTick_queueFullDoesNotReconnect(t *testing.T) {
	discoverCalled := false
	applyCalled := false
	w, cfg := testWatcher(t,
		&config.BridgePro{Host: "192.0.2.1", AppKey: "a"},
		func(*config.BridgePro) error {
			return fmt.Errorf("PUT /x: status 503: busy: %w", bridgepro.ErrQueueFull)
		},
		func() ([]bridgepro.DiscoveredBridge, error) { discoverCalled = true; return nil, nil },
		nil,
		func(*config.BridgePro) { applyCalled = true },
	)

	if w.tick() {
		t.Fatal("tick reported a reconnect on a 503 (queue full) — must NOT re-discover")
	}
	if discoverCalled {
		t.Fatal("discover called on a 503 (queue full): the Pro is reachable, just busy")
	}
	if applyCalled {
		t.Fatal("applyProvider called on a 503: the stored Pro must be untouched")
	}
	if cfg.GetPro().Host != "192.0.2.1" {
		t.Errorf("stored Pro changed on a 503: %q", cfg.GetPro().Host)
	}
}

func TestProWatcherTick_unreachableReconnects(t *testing.T) {
	calls := 0
	healthCheck := func(*config.BridgePro) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("GET /light: %w", bridgepro.ErrUnreachable)
		}
		return nil // the post-reconnect validation succeeds
	}
	discoverCalled := false
	fetchCalled := false
	applied := (*config.BridgePro)(nil)
	w, cfg := testWatcher(t,
		&config.BridgePro{Host: "192.0.2.1", AppKey: "a", ClientKey: "ck"},
		healthCheck,
		func() ([]bridgepro.DiscoveredBridge, error) {
			discoverCalled = true
			return []bridgepro.DiscoveredBridge{{ID: "b1", InternalIPAddress: "192.0.2.9"}}, nil
		},
		func(string) (string, error) { fetchCalled = true; return "newfp", nil },
		func(p *config.BridgePro) { applied = p },
	)
	w.skipTLS = false // exercise the cert-fetch path

	if !w.tick() {
		t.Fatal("tick did not report a reconnect for an unreachable Pro")
	}
	if !discoverCalled {
		t.Fatal("discover not called for an unreachable Pro")
	}
	if !fetchCalled {
		t.Fatal("fetchFingerprint not called during reconnect")
	}
	if applied == nil {
		t.Fatal("applyProvider not invoked on a committed reconnect")
	}
	if got := cfg.GetPro(); got.Host != "192.0.2.9" {
		t.Errorf("reconnected Pro host = %q, want 192.0.2.9", got.Host)
	}
	if cfg.GetPro().AppKey != "a" || cfg.GetPro().ClientKey != "ck" {
		t.Error("credentials lost across reconnect")
	}
}

func TestProWatcherTick_discoveryIDTargetsTheRightBridge(t *testing.T) {
	calls := 0
	w, cfg := testWatcher(t,
		// Stored DiscoveryID matches the SECOND discovered bridge.
		&config.BridgePro{Host: "192.0.2.1", AppKey: "a", DiscoveryID: "b2"},
		func(*config.BridgePro) error {
			calls++
			if calls == 1 {
				return fmt.Errorf("GET /light: %w", bridgepro.ErrUnreachable)
			}
			return nil
		},
		func() ([]bridgepro.DiscoveredBridge, error) {
			return []bridgepro.DiscoveredBridge{
				{ID: "b1", InternalIPAddress: "192.0.2.10"},
				{ID: "b2", InternalIPAddress: "192.0.2.20"},
			}, nil
		},
		nil, nil,
	)

	if !w.tick() {
		t.Fatal("tick did not reconnect")
	}
	if got := cfg.GetPro().Host; got != "192.0.2.20" {
		t.Errorf("reconnect targeted %q, want the Discovery-id-matched bridge 192.0.2.20 (not bridges[0])", got)
	}
	if cfg.GetPro().DiscoveryID != "b2" {
		t.Errorf("DiscoveryID = %q, want b2", cfg.GetPro().DiscoveryID)
	}
}

func TestProWatcherTick_storedDiscoveryIDNotFoundDoesNotReconnect(t *testing.T) {
	// The whole point of DiscoveryID: when the stored bridge is NOT among the
	// discovered ones, never reconnect to a DIFFERENT bridge (bridges[0]).
	discoverCalled := false
	applied := false
	w, cfg := testWatcher(t,
		&config.BridgePro{Host: "192.0.2.1", AppKey: "a", DiscoveryID: "b2"},
		func(*config.BridgePro) error { return fmt.Errorf("GET /light: %w", bridgepro.ErrUnreachable) },
		func() ([]bridgepro.DiscoveredBridge, error) {
			discoverCalled = true
			return []bridgepro.DiscoveredBridge{
				{ID: "b1", InternalIPAddress: "192.0.2.10"},
				{ID: "b3", InternalIPAddress: "192.0.2.30"},
			}, nil
		},
		nil,
		func(*config.BridgePro) { applied = true },
	)

	if w.tick() {
		t.Fatal("tick reconnected despite the stored DiscoveryID not matching any discovered bridge")
	}
	if !discoverCalled {
		t.Fatal("discover should have been called (the Pro was unreachable)")
	}
	if applied {
		t.Fatal("applyProvider invoked — must not retarget a different bridge")
	}
	if got := cfg.GetPro().Host; got != "192.0.2.1" {
		t.Errorf("Pro host changed to %q — must stay 192.0.2.1 (no wrong-bridge retarget)", got)
	}
}

func TestParseServeOptions_UIPortDefaultsDisabled(t *testing.T) {
	opts, err := parseServeOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.uiPort != 0 {
		t.Fatalf("ui-port default = %d, want 0 (disabled)", opts.uiPort)
	}
}

func TestParseServeOptions_UIPortSet(t *testing.T) {
	opts, err := parseServeOptions([]string{"-ui-port", "33300"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.uiPort != 33300 {
		t.Fatalf("ui-port = %d, want 33300", opts.uiPort)
	}
}

func TestParseServeOptions_UIFlag(t *testing.T) {
	off, err := parseServeOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if off.ui {
		t.Fatal("-ui should default to false")
	}
	on, err := parseServeOptions([]string{"-ui"})
	if err != nil {
		t.Fatal(err)
	}
	if !on.ui {
		t.Fatal("-ui should be true when set")
	}
}

func TestParseServeOptions_DefaultModeIsEntertainment(t *testing.T) {
	opts, err := parseServeOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.mode != "entertainment" {
		t.Fatalf("default mode = %q, want entertainment (REST is the explicit fallback)", opts.mode)
	}
}

func TestDeriveServeConfig(t *testing.T) {
	base := serveOptions{mode: "entertainment", httpPort: 80, controlledLightWindow: time.Minute}

	t.Run("invalid mode is rejected", func(t *testing.T) {
		o := base
		o.mode = "bogus"
		if _, err := deriveServeConfig(o); err == nil {
			t.Fatal("expected error for invalid mode")
		}
	})

	t.Run("ui port clashing with http port is rejected", func(t *testing.T) {
		o := base
		o.uiPort = 80
		if _, err := deriveServeConfig(o); err == nil {
			t.Fatal("expected error for ui/http port clash")
		}
	})

	t.Run("entertainment shortens the activity window", func(t *testing.T) {
		sc, err := deriveServeConfig(base)
		if err != nil {
			t.Fatal(err)
		}
		if !sc.entertainmentMode || sc.activityWindow != 10*time.Second {
			t.Fatalf("got %+v", sc)
		}
	})

	t.Run("rest uses the longer activity window", func(t *testing.T) {
		o := base
		o.mode = "rest"
		sc, _ := deriveServeConfig(o)
		if sc.entertainmentMode || sc.activityWindow != 30*time.Second {
			t.Fatalf("got %+v", sc)
		}
	})

	t.Run("controlled window is raised to exceed idle-off", func(t *testing.T) {
		o := base
		o.idleOffTimeout = 50 * time.Second // window 60s < 50+15=65s → raise
		sc, _ := deriveServeConfig(o)
		if !sc.windowRaised || sc.controlledWindow != 65*time.Second {
			t.Fatalf("expected raised window 65s, got %+v", sc)
		}
	})

	t.Run("controlled window left alone when already large enough", func(t *testing.T) {
		o := base
		o.idleOffTimeout = 30 * time.Second // window 60s >= 30+15=45s → keep
		sc, _ := deriveServeConfig(o)
		if sc.windowRaised || sc.controlledWindow != time.Minute {
			t.Fatalf("expected unchanged 60s window, got %+v", sc)
		}
	})
}

func TestUIPortFor(t *testing.T) {
	cases := []struct {
		name   string
		ui     bool
		uiPort int
		want   int
	}{
		{"disabled by default", false, 0, 0},
		{"-ui uses predefined port", true, 0, uiDefaultPort},
		{"-ui-port overrides without -ui", false, 8080, 8080},
		{"-ui-port wins over -ui", true, 8080, 8080},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uiPortFor(serveOptions{ui: tc.ui, uiPort: tc.uiPort})
			if got != tc.want {
				t.Fatalf("uiPortFor(ui=%v,uiPort=%d) = %d, want %d", tc.ui, tc.uiPort, got, tc.want)
			}
		})
	}
}
