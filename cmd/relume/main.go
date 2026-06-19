// Command relume is a software bridge that connects a Philips Ambilight TV to
// a Hue Bridge Pro by presenting itself to the TV as a Gen-2 bridge and
// forwarding commands to the Pro.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/trick77/relume/internal/bridge"
	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/clipv1"
	"github.com/trick77/relume/internal/config"
	"github.com/trick77/relume/internal/diag"
	"github.com/trick77/relume/internal/entertainment"
	"github.com/trick77/relume/internal/huestream"
	"github.com/trick77/relume/internal/mdns"
	"github.com/trick77/relume/internal/ssdp"
	"github.com/trick77/relume/internal/webui"
)

// version is set at build time via -ldflags "-X main.version=..." (CI).
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "version", "-version", "--version":
		fmt.Println("relume", version)
		return
	case "serve":
		if err := runServe(os.Args[2:], log); err != nil {
			log.Error("serve terminated", "err", err)
			os.Exit(1)
		}
	case "setup":
		if err := runSetup(os.Args[2:], log); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "discover":
		if err := runDiscover(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "avahi-service":
		if err := runAvahiService(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nAvailable: serve, setup, discover, avahi-service\n", cmd)
		os.Exit(2)
	}
}

type serveOptions struct {
	configPath             string
	httpPort               int
	advertiseIP            string
	debug                  bool
	tvIP                   string
	discoveryBurstDuration time.Duration
	discoveryBurstInterval time.Duration
	disableSSDP            bool
	bridgeIP               string
	skipTLS                bool
	idleOffTimeout         time.Duration
	controlledLightWindow  time.Duration
	mode                   string
	dtlsFallbackTimeout    time.Duration
	dtlsFallbackRecovery   time.Duration
	ui                     bool
	uiPort                 int
}

// uiDefaultPort is the fixed port the web UI listens on when enabled via -ui.
// -ui-port overrides it with a custom port.
const uiDefaultPort = 33100

// uiPortFor resolves the effective web UI port: -ui-port wins when set; otherwise
// -ui selects the predefined port; 0 means the UI is disabled.
func uiPortFor(opts serveOptions) int {
	if opts.uiPort != 0 {
		return opts.uiPort
	}
	if opts.ui {
		return uiDefaultPort
	}
	return 0
}

func parseServeOptions(args []string) (serveOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "relume.json", "path to the configuration file")
	httpPort := fs.Int("http-port", 80, "HTTP port of the emulated bridge")
	advIP := fs.String("advertise-ip", "", "advertised IP (empty = auto-detect)")
	debug := fs.Bool("debug", false, "verbose diagnostics: SSDP/HTTP datagrams + mDNS observer")
	tvIP := fs.String("tv-ip", "", "TV IP to log all mDNS questions from in debug mode")
	burstDuration := fs.Duration("discovery-burst-duration", 0, "send SSDP and mDNS discovery announcements at startup for this long")
	burstInterval := fs.Duration("discovery-burst-interval", time.Second, "interval for discovery-burst announcements")
	disableSSDP := fs.Bool("disable-ssdp", false, "do not run the SSDP responder (mDNS-only, like ha-hue-entertainment) — diagnostic")
	bridgeIP := fs.String("bridge-ip", "", "Bridge Pro IP for auto-pairing (empty = cloud discovery)")
	skipTLS := fs.Bool("skip-tls-verify", false, "skip TLS verification to the Bridge Pro (instead of cert pinning)")
	idleOffTimeout := fs.Duration("idle-off-timeout", 30*time.Second, "when the TV stops sending light writes for this long, flash the lights green twice and turn them off (0 = disabled)")
	controlledLightWindow := fs.Duration("controlled-light-window", time.Minute, "sliding window: a light counts as a current Ambilight light only if the TV drove it within this window; the restart/idle flash and idle-off touch only those (so config changes are forgotten after the window)")
	mode := fs.String("mode", "entertainment", "control mode: 'entertainment' (default, low-latency DTLS stream to the Pro; auto-falls back to REST if the TV never opens its stream) or 'rest' (per-light REST-follow)")
	dtlsFallbackTimeout := fs.Duration("entertainment-dtls-timeout", 5*time.Second, "entertainment mode: how long to wait after confirming the TV's stream activation for the TV to open its DTLS stream on :2100 before reverting to REST-follow")
	dtlsFallbackRecovery := fs.Duration("entertainment-fallback-recovery", 90*time.Second, "entertainment mode: how long a latched REST fallback persists before the next TV activation may recover it (0 disables: fallback stays sticky until restart)")
	ui := fs.Bool("ui", false, "enable the optional web UI on the predefined port 33100 (off by default)")
	uiPort := fs.Int("ui-port", 0, "override the web UI port (implies -ui; 0 = use -ui's default). Must differ from -http-port (80)")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	return serveOptions{
		configPath:             *cfgPath,
		httpPort:               *httpPort,
		advertiseIP:            *advIP,
		debug:                  *debug,
		tvIP:                   *tvIP,
		discoveryBurstDuration: *burstDuration,
		discoveryBurstInterval: *burstInterval,
		disableSSDP:            *disableSSDP,
		bridgeIP:               *bridgeIP,
		skipTLS:                *skipTLS,
		idleOffTimeout:         *idleOffTimeout,
		controlledLightWindow:  *controlledLightWindow,
		mode:                   *mode,
		dtlsFallbackTimeout:    *dtlsFallbackTimeout,
		dtlsFallbackRecovery:   *dtlsFallbackRecovery,
		ui:                     *ui,
		uiPort:                 *uiPort,
	}, nil
}

// serveConfig holds the derived, validated decisions for a serve run — separated
// from the goroutine wiring so the branching logic (mode, port clash, window sizing,
// summary cadence) is unit-testable without launching any listener.
type serveConfig struct {
	entertainmentMode bool          // control path: entertainment (DTLS) vs rest-follow
	controlledWindow  time.Duration // sliding window for the currently-driven lights
	windowRaised      bool          // true if controlledWindow was raised to exceed idle-off
	activityWindow    time.Duration // cadence of the periodic activity summary
	uiPort            int           // effective web UI port (0 = disabled)
}

// deriveServeConfig validates the serve options and computes the derived settings.
// It is pure (no I/O) so every branch is testable. Returns an error for an invalid
// mode or a web-UI port that clashes with the TV-facing HTTP port.
func deriveServeConfig(opts serveOptions) (serveConfig, error) {
	switch opts.mode {
	case "rest", "entertainment":
	default:
		return serveConfig{}, fmt.Errorf("invalid -mode %q (want 'rest' or 'entertainment')", opts.mode)
	}
	ent := opts.mode == "entertainment"

	uiPort := uiPortFor(opts)
	if uiPort != 0 && uiPort == opts.httpPort {
		return serveConfig{}, fmt.Errorf("web UI port %d clashes with -http-port; choose another (e.g. %d)", uiPort, uiDefaultPort)
	}

	// The controlled-light window must exceed the idle-off timeout, or the set would
	// already be empty by the time idle-off fires (nothing left to flash off).
	window := opts.controlledLightWindow
	raised := false
	if minWindow := opts.idleOffTimeout + 15*time.Second; opts.idleOffTimeout > 0 && window < minWindow {
		window = minWindow
		raised = true
	}

	// Entertainment mode shortens the summary window to surface the update-rate sooner.
	activityWindow := 30 * time.Second
	if ent {
		activityWindow = 10 * time.Second
	}

	return serveConfig{
		entertainmentMode: ent,
		controlledWindow:  window,
		windowRaised:      raised,
		activityWindow:    activityWindow,
		uiPort:            uiPort,
	}, nil
}

func runServe(args []string, log *slog.Logger) error {
	opts, err := parseServeOptions(args)
	if err != nil {
		return err
	}
	sc, err := deriveServeConfig(opts)
	if err != nil {
		return err
	}

	// Optional web UI (enabled via -ui on the predefined port, or -ui-port to
	// override it). When enabled, tee every log record into a hub so the UI's live
	// event tail mirrors stderr. When disabled, the logger is untouched and the
	// whole UI subsystem stays dormant (no overhead, headless behaviour).
	uiPort := sc.uiPort
	var uiHub *webui.Hub
	if uiPort != 0 {
		uiHub = webui.NewHub(200)
		base := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
		log = slog.New(webui.NewLogHandler(base, uiHub))
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	// Snapshot the paired Pro once, under the config mutex, before launching any
	// goroutine that can re-pair (autoPairPro) or reconnect (watchPro) it. SetPro
	// replaces the pointer rather than mutating in place, so this snapshot stays a
	// consistent, immutable view for all the startup wiring below; reads after
	// startup go through cfg.GetPro().
	pro := cfg.GetPro()

	ip := opts.advertiseIP
	if ip == "" {
		ip, err = outboundIP()
		if err != nil {
			return fmt.Errorf("auto-detect advertise-ip: %w (use -advertise-ip)", err)
		}
	}
	log.Info("relume", "version", version)
	log.Info("identity", "serial", cfg.Identity.Serial, "bridgeid", cfg.Identity.BridgeID(), "advertise", ip)
	// Dump the saved state on startup (no secrets): which Bridge Pro is paired and
	// which TVs are already paired. An already-paired TV explains an "instant"
	// re-pairing — POST /api then returns the stored user without the 5s delay.
	log.Info("saved config",
		"path", opts.configPath,
		"", pro, // LogValue inlines name/id/host (no "pro." prefix), or pro=<none> when unpaired
		"tv_paired", len(cfg.PairedDeviceTypes()),
		"tv_devicetypes", cfg.PairedDeviceTypes(),
	)

	// mode selects the control path (validated in deriveServeConfig). Entertainment
	// (default) confirms the TV's stream activation and runs the DTLS receiver on
	// :2100, streaming to the Pro over DTLS; if the TV never opens its stream the
	// watchdog reverts to REST. REST keeps the proven per-light REST-follow behavior.
	entertainmentMode := sc.entertainmentMode

	clip := clipv1.New(cfg, ip, opts.httpPort, log)
	clip.Debug = opts.debug
	clip.TVIP = opts.tvIP
	clip.EntertainmentMode = entertainmentMode
	clip.SetDTLSFallbackTimeout(opts.dtlsFallbackTimeout)
	clip.SetDTLSFallbackRecovery(opts.dtlsFallbackRecovery)
	log.Info("control mode", "mode", opts.mode)

	// controlled tracks the lights the TV is currently driving for Ambilight (a
	// sliding window). The restart/idle flash and idle-off target only these — and
	// nothing when the set is empty, so we never flash uncaptured lights. The window
	// was sized in deriveServeConfig to exceed the idle-off timeout.
	window := sc.controlledWindow
	if sc.windowRaised {
		log.Info("controlled-light-window raised to exceed idle-off-timeout", "window", window.String())
	}
	controlled := bridge.NewControlledSet(window)
	clip.ControlledLights = controlled.Current

	// liveColors records the latest colour the TV streamed per light (from both the
	// REST forward and the DTLS passthrough), so the web UI can show the live swatch
	// colour and mark driven lights even in pure DTLS mode. Harmless when no UI runs.
	// Its freshness window decides which lights count as driven RIGHT NOW: short
	// enough to empty soon after the stream stops, long enough to stay full across
	// frame jitter (DTLS streams ~50 Hz, REST writes a few Hz while content plays).
	// Deliberately separate from the ControlledSet window (which the idle/restart
	// flash relies on and must outlast the idle-off timeout).
	liveColors := newLiveColors(drivenLightWindow)

	// frameStats tracks the live entertainment frame rate (fed once per decoded TV
	// frame below) so the web UI can show the stream's frames/s. Harmless when no UI
	// runs or no DTLS stream is active (reports 0).
	frameStats := newFrameStats()
	// proSendStats tracks relume's *outgoing* DTLS rate to the Pro (50 Hz sendLoop),
	// the counterpart to frameStats' incoming rate. proStats bundles the REST-path
	// counters: write rate, coalesced drops/s, and cumulative forward errors. Only
	// the DTLS or the REST counters are non-zero at a time, depending on the path.
	proSendStats := newFrameStats()
	proStats := newProStats()

	if pro != nil {
		client := bridgepro.New(pro)
		clip.SetLightProvider(newProvider(client, controlled, liveColors, proStats, log))
		log.Info("hue bridge pro paired", "", pro)
	}
	var responder *ssdp.Responder
	if opts.disableSSDP {
		log.Info("ssdp: disabled (mDNS-only mode)")
	} else {
		responder = ssdp.New(cfg.Identity, ip, opts.httpPort, log)
		responder.Debug = opts.debug
		responder.BurstDuration = opts.discoveryBurstDuration
		responder.BurstInterval = opts.discoveryBurstInterval
	}
	announcer := mdns.New(cfg.Identity, ip, opts.httpPort, log)
	announcer.BurstDuration = opts.discoveryBurstDuration
	announcer.BurstInterval = opts.discoveryBurstInterval

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Pair the Bridge Pro backend in the background, independently of the TV: the
	// TV can discover/pair relume before the Pro is paired; relume just returns an
	// empty light list until the Pro pairing completes, then hot-loads the lights.
	if pro == nil {
		log.Warn("no hue bridge pro paired yet – auto-pairing in background; TAP the hue bridge pro link button")
		go autoPairPro(ctx, cfg, clip, controlled, liveColors, proStats, opts.bridgeIP, opts.skipTLS, log)
	} else {
		// No restart flash at startup: the controlled set is empty here (no TV write
		// captured yet), so we have nothing to flash. The restart indicator is the
		// shutdown flash below — on `docker compose up -d` the old container gets
		// SIGTERM and blinks the currently-driven Ambilight bulbs red+off first.
		// Keep the already-paired Pro reachable across reboots / IP changes.
		w := newProWatcher(cfg, clip, controlled, liveColors, proStats, opts.bridgeIP, opts.skipTLS, log)
		go w.run(ctx)
	}

	// Optional web UI (opt-in via -ui / -ui-port). Read-only: it reads live state
	// via the uiSource adapter. A bind/serve failure is logged but never takes down
	// the headless service.
	if uiPort != 0 {
		bridgeID := cfg.Identity.BridgeID()
		src := &uiSource{
			cfg:          cfg,
			clip:         clip,
			liveColors:   liveColors,
			frameStats:   frameStats,
			proSendStats: proSendStats,
			proStats:     proStats,
			// UI-only display name for relume's own bridge. NOTE: the actual mDNS
			// instance the TV discovers is still "Philips Hue - …" (internal/mdns),
			// deliberately unchanged so discovery keeps working.
			advName: "Relume Bridge - " + bridgeID[len(bridgeID)-6:],
			version: version,
			started: time.Now(),
		}
		uiSrv := webui.NewServer(fmt.Sprintf(":%d", uiPort), uiHub, src, log)
		go func() {
			if err := uiSrv.Run(ctx); err != nil {
				log.Warn("web ui server stopped", "err", err)
			}
		}()
		log.Info("web ui enabled", "addr", fmt.Sprintf(":%d", uiPort))
	}

	// Summarize the high-frequency Ambilight light-state writes periodically instead
	// of logging every single request (cadence derived in deriveServeConfig).
	go clip.LogActivitySummary(ctx, sc.activityWindow)

	// entStreamer is hoisted to function scope so the shutdown path can release
	// relume's own entertainment stream on the Pro synchronously (Phase D), rather
	// than racing the receiver's async OnStreamStop against process exit.
	var entStreamer *entertainment.ProStreamer

	if entertainmentMode {
		// Entertainment mode: run the real DTLS receiver on :2100. It decrypts the
		// TV's stream (PSK = the clientkey relume minted at pairing) and decodes the
		// HueStream frames.
		recv := entertainment.NewReceiver(ip, cfg.PSKForUser, log)
		// Count stream frames as activity so the idle-off monitor doesn't flash the
		// lights off mid-stream (the TV streams via DTLS, not REST writes, here), and
		// record each frame's arrival so the UI can show the live frame rate.
		recv.OnActivity = func() {
			clip.MarkActivity()
			frameStats.Mark()
		}

		if pro != nil {
			// Phase C: relume opens its OWN entertainment stream to the Pro over DTLS
			// and re-encodes the decoded TV frames at full rate, avoiding the per-light
			// REST writes that overflow the Pro's command queue (503). The streamer
			// auto-falls back to the REST forward (Phase B) if DTLS cannot establish.
			proClient := bridgepro.New(pro)
			clientKey, _ := hex.DecodeString(pro.ClientKey)
			streamer := entertainment.NewProStreamer(proClient, pro.Host, pro.AppKey, clientKey, clip.ForwardLight, log)
			// Surface the live DTLS-passthrough colours to the web UI (the REST path is
			// covered by the provider's OnColor via the fallback sink).
			streamer.OnColor = liveColors.SetStates
			// Record each frame sent to the Pro over DTLS so the UI can show the live
			// outgoing send rate (the 50 Hz counterpart to the TV's ~25 Hz input).
			streamer.OnSend = proSendStats.Mark
			// Honor the TV's group membership: when the TV declares which lights belong to
			// its Ambilight zone (POST/PUT /groups), restrict the Pro config to that
			// subset so lights in other rooms are never driven.
			clip.OnGroupMembers = streamer.SetRequestedMembers
			// Persist/reuse relume's own entertainment_configuration across restarts
			// instead of re-finding it each stream (Phase D).
			streamer.SetConfigStore(cfg.LoadEntConfigID, func(id string) {
				if err := cfg.SaveEntConfigID(id); err != nil {
					log.Warn("persisting relume entertainment config id", "err", err)
				}
			})
			entStreamer = streamer
			// The TV opening its DTLS stream cancels the activation-fallback watchdog
			// (clip) and establishes the Pro stream (streamer).
			recv.OnStreamStart = func(remote string) {
				clip.MarkDTLSStreamUp()
				streamer.Start(remote)
			}
			recv.OnStreamStop = func(remote string) {
				clip.MarkDTLSStreamDown()
				streamer.Stop(remote)
			}
			recv.OnFrame = streamer.Push
			log.Info("entertainment mode: DTLS receiver on udp :2100 → streaming to the hue bridge pro over DTLS (REST fallback)")
		} else {
			// No Pro paired yet: forward decoded frames to the Pro via the coalescing
			// REST provider (Phase B) until pairing completes. The channel id IS the v1
			// light id the TV referenced in its entertainment group.
			recv.OnStreamStart = func(string) { clip.MarkDTLSStreamUp() }
			recv.OnStreamStop = func(string) { clip.MarkDTLSStreamDown() }
			recv.OnFrame = func(_ string, f *huestream.Frame) {
				for _, ch := range f.Channels {
					// Honor the TV subset here too (defense in depth): ch.ID is the v1
					// light id; skip channels outside the TV's requested Ambilight set.
					if !clip.AllowsMember(ch.ID) {
						continue
					}
					clip.ForwardLight(strconv.Itoa(int(ch.ID)), entertainment.ToHueV1State(f.ColorSpace, ch))
				}
			}
			log.Info("entertainment mode: DTLS receiver on udp :2100 → REST forward (no hue bridge pro paired yet)")
		}
		go func() {
			if err := recv.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("entertainment receiver", "err", err)
			}
		}()
	}

	// Detect the TV going silent (switched off / control session broke) and flash
	// the lights green twice, then off — the TV sends no off signal, it just stops
	// writing. Disabled when the timeout is 0.
	if opts.idleOffTimeout > 0 {
		log.Info("idle-off monitor active", "timeout", opts.idleOffTimeout.String())
		go monitorIdle(ctx, clip, cfg, controlled, opts.idleOffTimeout, log)
	}

	go func() {
		if err := announcer.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("mdns announcer", "err", err)
		}
	}()

	if opts.debug {
		obs := diag.NewMDNSObserver(ip, log)
		obs.DebugTVIP = opts.tvIP
		go func() {
			if err := obs.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("mdns observer", "err", err)
			}
		}()
		log.Info("debug mode active: SSDP/HTTP diagnostics + mDNS observer", "tvIP", opts.tvIP)
	}

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", opts.httpPort),
		Handler: clip.Handler(),
	}

	errc := make(chan error, 2)
	go func() {
		log.Info("http server started", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- fmt.Errorf("http: %w", err)
		}
	}()
	if responder != nil {
		go func() {
			if err := responder.Run(ctx); err != nil && ctx.Err() == nil {
				errc <- fmt.Errorf("ssdp: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal")
	case err := <-errc:
		stop()
		shutdownHTTP(httpSrv)
		stopEntertainment(entStreamer)
		return err
	}
	shutdownHTTP(httpSrv)
	// Release relume's own entertainment stream on the Pro before flashing, so the
	// Pro area is deactivated (StopStream uses the client's own timeout, not the
	// cancelled ctx) and never leaks past process exit (Phase D).
	stopEntertainment(entStreamer)
	// Stop accepting TV writes first (above), then signal the restart on the lights.
	// GetPro reads the current pairing under the mutex (watchPro may have reconnected
	// it to a new IP since startup).
	if shutPro := cfg.GetPro(); shutPro != nil {
		flashRestartBounded(bridgepro.New(shutPro), log, inZoneUUIDs(clip, controlled.Current()), shutdownFlashBudget)
	}
	return nil
}

// shutdownFlashBudget caps the total time the restart flash may take on shutdown.
// FlashRestart runs synchronously and each light write blocks on the Bridge Pro's
// HTTP timeout; if the Pro is unreachable at shutdown, an unbounded flash would
// stall well past the container stop grace (then get SIGKILLed). This bounds it so
// the process exits promptly.
const shutdownFlashBudget = 3 * time.Second

// flashRestartBounded runs the restart flash with a hard total deadline: it returns
// after the flash completes or after max elapses (whichever first). On timeout the
// flash goroutine is abandoned — the process is exiting anyway.
func flashRestartBounded(client *bridgepro.Client, log *slog.Logger, ids []string, max time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridge.FlashRestart(client, log, ids)
	}()
	select {
	case <-done:
	case <-time.After(max):
		log.Warn("restart flash timed out (hue bridge pro unreachable?) — exiting anyway", "after", max.String())
	}
}

// zoneMembership is the slice of the clipv1 server inZoneUUIDs needs: resolve a Pro
// UUID to its v1 id and ask whether that id is in the TV's current Ambilight zone.
// *clipv1.Server satisfies it; an interface keeps the filter testable in isolation.
type zoneMembership interface {
	V1ForUUID(uuid string) (string, bool)
	AllowsMember(v1id uint16) bool
}

// inZoneUUIDs filters a flash target (Bridge Pro light UUIDs) down to the lights
// still in the TV's current Ambilight zone, so the restart/idle flash never touches
// an off-zone light — even one that lingers in the ControlledSet from before the zone
// shrank. A UUID is dropped only when its v1 id resolves AND is positively outside the
// zone; an unresolved UUID is kept, mirroring AllowsMember's defensive "allow when
// unknown" stance so the flash is never silently reduced to nothing. With no zone
// declared (AllowsMember true for all) the list passes through unchanged.
func inZoneUUIDs(m zoneMembership, uuids []string) []string {
	out := make([]string, 0, len(uuids))
	for _, uuid := range uuids {
		if v1, ok := m.V1ForUUID(uuid); ok {
			if n, err := strconv.Atoi(v1); err == nil && !m.AllowsMember(uint16(n)) {
				continue
			}
		}
		out = append(out, uuid)
	}
	return out
}

// stopEntertainment tears down the Pro entertainment stream on shutdown (idempotent
// and nil-safe — the streamer only exists in entertainment mode with a paired Pro).
func stopEntertainment(s *entertainment.ProStreamer) {
	if s != nil {
		s.Stop("shutdown")
	}
}

// newProvider builds the Bridge Pro light provider and wires it to feed the
// sliding-window ControlledSet with each light the TV drives, so the restart/idle
// flash and idle-off touch only the bulbs the TV is currently driving — never the
// rest of the home.
func newProvider(client *bridgepro.Client, controlled *bridge.ControlledSet, live *liveColors, stats *proStats, log *slog.Logger) *bridge.LightProvider {
	p := bridge.NewLightProvider(client, log)
	p.OnControlled = controlled.Seen
	p.OnColor = live.SetState
	// Record each successful REST write to the Pro so the UI can show the live
	// outgoing write rate when relume drives the Pro over REST (no DTLS stream).
	p.OnForward = stats.writes.Mark
	// Backpressure signals for the UI: coalesced (healthy drops/s the optimistic
	// path spared the Pro) and forward errors (the real failure signal, cumulative).
	p.OnCoalesce = stats.coalesces.Mark
	p.OnForwardErr = func() { stats.fwdErrs.Add(1) }
	return p
}

func shutdownHTTP(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// monitorIdle watches the TV's Ambilight write activity and, once it has gone
// silent for idleTimeout after having been active, flashes the Bridge Pro lights
// green twice and turns them off (bridge.FlashIdle). The TV sends no explicit
// off signal — it just stops writing — so this inactivity timeout stands in for
// it. It fires once per active→idle transition and re-arms when the TV resumes
// writing. The flash is a no-op while no Pro is paired or the Pro is unreachable.
func monitorIdle(ctx context.Context, clip *clipv1.Server, cfg *config.Config, controlled *bridge.ControlledSet, idleTimeout time.Duration, log *slog.Logger) {
	interval := 2 * time.Second
	if idleTimeout < interval {
		interval = idleTimeout
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	var lastSeen time.Time
	fired := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			act := clip.LastActivity()
			if act.After(lastSeen) {
				// New activity since the last observation → re-arm.
				lastSeen, fired = act, false
				continue
			}
			if !idleShouldFire(now, lastSeen, fired, idleTimeout) {
				continue
			}
			fired = true
			// GetPro reads the pairing under its mutex — autoPairPro/watchPro may
			// re-pair concurrently. nil while no Pro is paired: nothing to flash.
			if pro := cfg.GetPro(); pro != nil {
				log.Info("ambilight idle: flashing the ambilight lights off", "idle_for", now.Sub(lastSeen).Round(time.Second).String())
				bridge.FlashIdle(bridgepro.New(pro), log, inZoneUUIDs(clip, controlled.Current()))
			}
		}
	}
}

// idleShouldFire reports whether the idle-off flash should fire this tick: the TV
// has been active at least once (lastSeen non-zero), has now been silent for the
// timeout, and the flash has not already fired for this active→idle transition.
func idleShouldFire(now, lastSeen time.Time, fired bool, idleTimeout time.Duration) bool {
	return !fired && !lastSeen.IsZero() && now.Sub(lastSeen) >= idleTimeout
}

// autoPairPro pairs relume with the Bridge Pro in the background, independently of
// the TV side. It discovers the Pro (cloud, unless bridgeIP is given), pins the
// leaf certificate, then polls until the user taps the Pro's physical link button
// (the one step that cannot be automated). On success it persists the credentials
// and hot-loads the light backend so the already-paired TV starts seeing lights.
func autoPairPro(ctx context.Context, cfg *config.Config, clip *clipv1.Server, controlled *bridge.ControlledSet, live *liveColors, stats *proStats, bridgeIP string, skipTLS bool, log *slog.Logger) {
	var host, discoveryID string
	for host == "" {
		h, id, derr := resolveProHost(bridgeIP, "", bridgepro.Discover, log)
		if derr == nil && h != "" {
			host, discoveryID = h, id
			// Make the source explicit: with no -bridge-ip the host comes from the
			// Philips cloud (discovery.meethue.com), so a config-less relume still
			// "knows" a bridge it never stored — that is the cloud cache, not local state.
			source := "Philips cloud (discovery.meethue.com)"
			if bridgeIP != "" {
				source = "-bridge-ip"
			}
			log.Info("hue bridge pro discovered", "host", host, "via", source)
			break
		}
		log.Warn("hue bridge pro not paired yet: not found via cloud discovery — power the hue bridge pro on, or pass -bridge-ip; retrying", "err", derr)
		if !sleepCtx(ctx, 15*time.Second) {
			return
		}
	}

	var pro *config.BridgePro
	for pro == nil {
		p, ferr := pinProShell(host, discoveryID, skipTLS, bridgepro.FetchLeafFingerprint)
		if ferr == nil {
			pro = p
			if pro.CertSHA256 != "" {
				log.Info("hue bridge pro certificate pinned", "sha256", pro.CertSHA256)
			}
			break
		}
		log.Warn("hue bridge pro not paired yet: cannot reach it to pin its certificate — power the hue bridge pro on; retrying", "host", host, "err", ferr)
		if !sleepCtx(ctx, 15*time.Second) {
			return
		}
	}

	log.Info("waiting for the hue bridge pro link button — TAP it now", "host", host)
	paired, perr := bridgepro.NewPairer("relume#"+hostname()).
		WaitForLinkButton(ctx, pro, time.Time{}, func(attempt int) {
			if attempt%6 == 0 {
				log.Info("still waiting for the hue bridge pro link button — TAP it", "host", host)
			}
		})
	if perr != nil {
		return // ctx cancelled during the wait
	}
	if serr := cfg.SetPro(paired); serr != nil {
		log.Error("persisting hue bridge pro pairing", "err", serr)
		return
	}
	client := bridgepro.New(paired)
	clip.SetLightProvider(newProvider(client, controlled, live, stats, log))
	log.Info("hue bridge pro paired (auto)", "", paired)
	if lights, lerr := client.Lights(); lerr == nil {
		log.Info("hue bridge pro lights available", "count", len(lights), "color", colorCapable(lights))
	}
}

// reconnectProConfig builds the Bridge Pro config for a reconnect: it keeps the
// existing credentials (appKey/clientKey — valid across reboots and IP changes,
// so no re-pairing) and refreshes only the host and pinned certificate. The
// DiscoveryID carries forward like Name/BridgeID (it identifies the SAME bridge
// across the reconnect); the caller may overwrite it when a fresh discovery
// returned the matched bridge's id.
func reconnectProConfig(old *config.BridgePro, host, certSHA256 string, skipTLS bool) *config.BridgePro {
	return &config.BridgePro{
		Host:          host,
		AppKey:        old.AppKey,
		ClientKey:     old.ClientKey,
		CertSHA256:    certSHA256,
		SkipTLSVerify: skipTLS || old.SkipTLSVerify,
		Name:          old.Name,
		BridgeID:      old.BridgeID,
		DiscoveryID:   old.DiscoveryID,
	}
}

// sleepCtx sleeps for d or until ctx is cancelled; returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// runDiscover lists bridges found on the local network via the Philips cloud.
func runDiscover() error {
	bridges, err := bridgepro.Discover()
	if err != nil {
		return err
	}
	if len(bridges) == 0 {
		fmt.Println("No bridges found (cloud discovery). Use setup -bridge-ip <ip>.")
		return nil
	}
	fmt.Println("Bridges found:")
	for _, b := range bridges {
		fmt.Printf("  id=%s  ip=%s\n", b.ID, b.InternalIPAddress)
	}
	return nil
}

// runAvahiService prints an Avahi static service file with which a Linux host
// running avahi-daemon announces the _hue._tcp service. Needed when avahi
// occupies port 5353 and relume's built-in mDNS announcer therefore can't bind:
//
//	relume avahi-service > /etc/avahi/services/relume-hue.service
func runAvahiService(args []string) error {
	fs := flag.NewFlagSet("avahi-service", flag.ExitOnError)
	cfgPath := fs.String("config", "relume.json", "path to the configuration file")
	port := fs.Int("http-port", 80, "advertised port (must match the serve http-port)")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	bridgeID := cfg.Identity.BridgeID()
	instance := "Philips Hue - " + bridgeID[len(bridgeID)-6:]
	fmt.Printf(`<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name>%s</name>
  <service>
    <type>_hue._tcp</type>
    <port>%d</port>
    <txt-record>bridgeid=%s</txt-record>
    <txt-record>modelid=BSB002</txt-record>
  </service>
</service-group>
`, instance, *port, bridgeID)
	return nil
}

// runSetup pairs relume with the real Hue Bridge Pro: pin the certificate,
// wait for the link button, fetch the app key + clientkey and persist them.
func runSetup(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", "relume.json", "path to the configuration file")
	bridgeIP := fs.String("bridge-ip", "", "IP of the Hue Bridge Pro (empty = cloud discovery)")
	skipTLS := fs.Bool("skip-tls-verify", false, "disable TLS verification against the Pro (instead of cert pinning)")
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait for the link button")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	host, discoveryID, derr := resolveProHost(*bridgeIP, "", bridgepro.Discover, log)
	if derr != nil || host == "" {
		return fmt.Errorf("no bridge found; please specify -bridge-ip (discover: %v)", derr)
	}
	if *bridgeIP == "" {
		fmt.Printf("Bridge found via cloud discovery: %s\n", host)
	}

	pro, ferr := pinProShell(host, discoveryID, *skipTLS, bridgepro.FetchLeafFingerprint)
	if ferr != nil {
		return fmt.Errorf("pin certificate: %w", ferr)
	}
	if pro.CertSHA256 != "" {
		log.Info("certificate pinned", "sha256", pro.CertSHA256)
	}

	fmt.Printf("\n>>> Now press the link button on the Hue Bridge Pro (%s) <<<\n\n", host)

	pairer := bridgepro.NewPairer("relume#" + hostname())
	pairer.Interval = 2 * time.Second
	paired, perr := pairer.WaitForLinkButton(context.Background(), pro, time.Now().Add(*timeout),
		func(int) { fmt.Print(".") })
	fmt.Println()
	if perr != nil {
		return fmt.Errorf("pairing failed (press the link button in time): %w", perr)
	}

	if err := cfg.SetPro(paired); err != nil {
		return err
	}
	fmt.Println("Pairing successful, app key saved.")

	// List lights as confirmation.
	client := bridgepro.New(paired)
	lights, lerr := client.Lights()
	if lerr != nil {
		fmt.Printf("Note: lights could not be read: %v\n", lerr)
		return nil
	}
	fmt.Printf("%d lights found, %d color-capable:\n", len(lights), colorCapable(lights))
	for _, l := range lights {
		fmt.Printf("  - %s (%s)\n", l.Metadata.Name, l.ID)
	}
	return nil
}

// colorCapable counts the lights usable for Ambilight: only color-capable bulbs
// are offered to the TV (translate.LightsV1 filters the rest), so this reports how
// many of the discovered lights are really available.
func colorCapable(lights []bridgepro.Light) int {
	n := 0
	for _, l := range lights {
		if l.HasColor() {
			n++
		}
	}
	return n
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "host"
	}
	return h
}

// outboundIP determines the local IPv4 over which outbound traffic flows.
func outboundIP() (string, error) {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, no real traffic
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
