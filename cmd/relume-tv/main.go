// Command relume-tv is a software bridge that connects a Philips Ambilight TV to
// a Hue Bridge Pro by presenting itself to the TV as a Gen-2 bridge and
// forwarding commands to the Pro.
package main

import (
	"context"
	"encoding/hex"
	"errors"
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

	"github.com/trick77/relume-tv/internal/bridge"
	"github.com/trick77/relume-tv/internal/bridgepro"
	"github.com/trick77/relume-tv/internal/clipv1"
	"github.com/trick77/relume-tv/internal/config"
	"github.com/trick77/relume-tv/internal/diag"
	"github.com/trick77/relume-tv/internal/entertainment"
	"github.com/trick77/relume-tv/internal/mdns"
	"github.com/trick77/relume-tv/internal/ssdp"
	"github.com/trick77/relume-tv/internal/webui"
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
		fmt.Println("relume-tv", version)
		return
	case "serve":
		if err := runServe(os.Args[2:], log); err != nil {
			log.Error("serve terminated", "err", err)
			os.Exit(1)
		}
	case "avahi-service":
		if err := runAvahiService(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nAvailable: serve, avahi-service\n", cmd)
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
	skipTLS                bool
	idleOffRest            time.Duration
	idleOffEntertainment   time.Duration
	controlledLightWindow  time.Duration
	mode                   string
	dtlsFallbackTimeout    time.Duration
	dtlsFallbackRecovery   time.Duration
	smoothTau              time.Duration
	headless               bool
	ui                     bool
	uiPort                 int
}

// uiDefaultPort is the fixed port the web UI listens on. -ui-port overrides it
// with a custom port; -headless disables the UI entirely.
const uiDefaultPort = 33100

// uiPortFor resolves the effective web UI port. The UI is ON by default: -headless
// turns it off (and wins over -ui-port); otherwise -ui-port overrides the predefined
// port; with neither, the predefined port is used. 0 means the UI is disabled.
func uiPortFor(opts serveOptions) int {
	if opts.headless {
		return 0
	}
	if opts.uiPort != 0 {
		return opts.uiPort
	}
	return uiDefaultPort
}

// defaultIdleOffRest / defaultIdleOffEntertainment are the per-mode idle-off defaults.
// REST writes are sparse (a few Hz, and pause on static scenes), so a longer timeout
// avoids turning the lights off mid-viewing; the entertainment DTLS stream is ~50 Hz
// and stops cleanly when the TV goes off, so a short timeout is safe and snappy. Only
// the active mode's value is used (selected in deriveServeConfig).
const (
	defaultIdleOffRest          = 30 * time.Second
	defaultIdleOffEntertainment = 5 * time.Second
)

func parseServeOptions(args []string) (serveOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "relume-tv.json", "path to the configuration file")
	httpPort := fs.Int("http-port", 80, "HTTP port of the emulated bridge")
	advIP := fs.String("advertise-ip", "", "advertised IP (empty = auto-detect)")
	debug := fs.Bool("debug", false, "verbose diagnostics: SSDP/HTTP datagrams + mDNS observer")
	tvIP := fs.String("tv-ip", "", "TV IP to log all mDNS questions from in debug mode")
	burstDuration := fs.Duration("discovery-burst-duration", 0, "send SSDP and mDNS discovery announcements at startup for this long")
	burstInterval := fs.Duration("discovery-burst-interval", time.Second, "interval for discovery-burst announcements")
	disableSSDP := fs.Bool("disable-ssdp", false, "do not run the SSDP responder (mDNS-only, like ha-hue-entertainment) — diagnostic")
	skipTLS := fs.Bool("skip-tls-verify", false, "skip TLS verification to the Hue Bridge Pro (instead of cert pinning)")
	idleOffRest := fs.Duration("idle-off-timeout-rest", defaultIdleOffRest, "rest mode: when the TV stops sending light writes for this long, turn the lights off (0 = disabled)")
	idleOffEntertainment := fs.Duration("idle-off-timeout-entertainment", defaultIdleOffEntertainment, "entertainment mode: when the TV stops streaming/writing for this long, turn the lights off (0 = disabled)")
	controlledLightWindow := fs.Duration("controlled-light-window", time.Minute, "sliding window: a light counts as a current Ambilight light only if the TV drove it within this window; the restart/idle turn-off touches only those (so config changes are forgotten after the window)")
	mode := fs.String("mode", "entertainment", "control mode: 'entertainment' (default, low-latency DTLS stream to the Pro; auto-falls back to REST if the TV never opens its stream) or 'rest' (per-light REST-follow)")
	dtlsFallbackTimeout := fs.Duration("entertainment-dtls-timeout", 5*time.Second, "entertainment mode: how long to wait after confirming the TV's stream activation for the TV to open its DTLS stream on :2100 before reverting to REST-follow")
	dtlsFallbackRecovery := fs.Duration("entertainment-fallback-recovery", 90*time.Second, "entertainment mode: how long a latched REST fallback persists before the next TV activation may recover it (0 disables: fallback stays sticky until restart)")
	smoothTau := fs.Duration("entertainment-smooth-tau", entertainment.DefaultSmoothTau, "entertainment mode: exponential-smoothing time constant for easing the TV's hard scene cuts on the DTLS send path. Lower = snappier but more flicker, higher = smoother but laggier; 0 disables smoothing (frames forwarded verbatim)")
	headless := fs.Bool("headless", false, "disable the web UI (it runs on the predefined port 33100 by default). NOTE: with network_mode: host the UI is otherwise reachable, unauthenticated, by anyone on the LAN")
	ui := fs.Bool("ui", false, "deprecated no-op: the web UI is on by default now (kept so existing -ui invocations still parse). Use -headless to turn it off")
	uiPort := fs.Int("ui-port", 0, "override the web UI port (0 = the predefined port 33100). Must differ from -http-port (80). Ignored when -headless is set")
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
		skipTLS:                *skipTLS,
		idleOffRest:            *idleOffRest,
		idleOffEntertainment:   *idleOffEntertainment,
		controlledLightWindow:  *controlledLightWindow,
		mode:                   *mode,
		dtlsFallbackTimeout:    *dtlsFallbackTimeout,
		dtlsFallbackRecovery:   *dtlsFallbackRecovery,
		smoothTau:              *smoothTau,
		headless:               *headless,
		ui:                     *ui,
		uiPort:                 *uiPort,
	}, nil
}

// serveConfig holds the derived, validated decisions for a serve run — separated
// from the goroutine wiring so the branching logic (mode, port clash, window sizing,
// summary cadence) is unit-testable without launching any listener.
type serveConfig struct {
	entertainmentMode bool          // control path: entertainment (DTLS) vs rest-follow
	idleOff           time.Duration // idle-off timeout selected for the active mode (0 = disabled)
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

	// Select the idle-off timeout for the active control mode (0 = disabled).
	idleOff := opts.idleOffRest
	if ent {
		idleOff = opts.idleOffEntertainment
	}

	// The controlled-light window must exceed the idle-off timeout, or the set would
	// already be empty by the time idle-off fires (nothing left to turn off).
	window := opts.controlledLightWindow
	raised := false
	if minWindow := idleOff + 15*time.Second; idleOff > 0 && window < minWindow {
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
		idleOff:           idleOff,
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

	// Web UI (on by default on the predefined port; -ui-port overrides it, -headless
	// disables it). When enabled, tee every log record into a hub so the UI's live
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
	log.Info("relume-tv", "version", version)
	log.Info("identity", "serial", cfg.Identity.Serial, "bridgeid", cfg.Identity.BridgeID(), "advertise", ip)
	// Dump the saved state on startup (no secrets): which Hue Bridge Pro is paired and
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
	// sliding window). The restart/idle turn-off targets only these — and
	// nothing when the set is empty, so we never turn off uncaptured lights. The window
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
	// turn-off relies on and must outlast the idle-off timeout).
	liveColors := newLiveColors(drivenLightWindow)

	// frameStats tracks the live entertainment frame rate (fed once per decoded TV
	// frame below) so the web UI can show the stream's frames/s. Harmless when no UI
	// runs or no DTLS stream is active (reports 0).
	frameStats := newFrameStats()
	// proSendStats tracks relume-tv's *outgoing* DTLS rate to the Pro (50 Hz sendLoop),
	// the counterpart to frameStats' incoming rate. proStats bundles the REST-path
	// counters: write rate, coalesced drops/s, and cumulative forward errors. Only
	// the DTLS or the REST counters are non-zero at a time, depending on the path.
	proSendStats := newFrameStats()
	// restRecvStats tracks the rate of inbound REST control calls the TV sends relume-tv
	// (per-light state PUTs + group-action PUTs), the REST-path counterpart to frameStats'
	// incoming DTLS rate. Surfaced as the UI "Received" card's REST reading; 0 unless the TV
	// is driving over REST.
	restRecvStats := newFrameStats()
	proStats := newProStats()
	// jitterStats holds the per-window brightness jump on the incoming TV stream vs
	// relume-tv's smoothed sent stream, so the UI can show how much the DTLS-path easing
	// cut the flicker. Fed by the receiver (input) and streamer (sent) rollups below.
	jitterStats := newJitterStats()

	// setup is the backend state machine driving the wizard. It is shared by the web UI
	// (a pure renderer) and headless operation (logs every transition), and triggers the
	// one-shot config Commit() when the first TV data flows (step 6). active() reuses the
	// same windowed "TV driving lights right now" signal the UI shows. Always created so
	// uiSource can read it; on a committed install it starts at stepDone (dashboard).
	setup := newSetupStatus(cfg, func() bool { return len(liveColors.DrivenV1IDs()) > 0 }, cfg.Commit, log)
	// The TV re-fetches /description.xml after a reboot — the step-2 signal. Wired in both
	// UI and headless modes (markTVDescriptorSeen only latches while step 2 is active).
	clip.OnDescriptorFetch = setup.markTVDescriptorSeen

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

	// Pair the Hue Bridge Pro backend in the background, independently of the TV: the
	// TV can discover/pair relume-tv before the Pro is paired; relume-tv just returns an
	// empty light list until the Pro pairing completes, then hot-loads the lights.
	if pro == nil {
		log.Warn("no hue bridge pro paired yet – auto-pairing in background; TAP the hue bridge pro link button")
		go autoPairPro(ctx, cfg, clip, controlled, liveColors, proStats, setup, opts.skipTLS, log)
	} else {
		// No restart turn-off at startup: the controlled set is empty here (no TV write
		// captured yet), so we have nothing to turn off. The lights are turned off on
		// shutdown below — on `docker compose up -d` the old container gets SIGTERM and
		// turns the currently-driven Ambilight bulbs off first.
		// Keep the already-paired Pro reachable across reboots / IP changes.
		w := newProWatcher(cfg, clip, controlled, liveColors, proStats, opts.skipTLS, log)
		go w.run(ctx)
	}

	// Web UI (on by default; -ui-port to move it, -headless to disable). Read-only: it
	// reads live state via the uiSource adapter. A bind/serve failure is logged but
	// never takes down the headless service.
	if uiPort != 0 {
		bridgeID := cfg.Identity.BridgeID()
		src := &uiSource{
			cfg:           cfg,
			clip:          clip,
			liveColors:    liveColors,
			frameStats:    frameStats,
			proSendStats:  proSendStats,
			restRecvStats: restRecvStats,
			proStats:      proStats,
			jitterStats:   jitterStats,
			setup:         setup,
			// The name the TV shows for this bridge — matches the TV-facing /config
			// name and UPnP friendlyName ("relume-tv-XXXXXX", a single token; the TV
			// truncates at the first space). NOTE: the mDNS instance the TV discovers
			// is still "Philips Hue - …" (internal/mdns), deliberately unchanged so
			// discovery keeps working.
			advName: "relume-tv-" + bridgeID[len(bridgeID)-6:],
			version: version,
			started: time.Now(),
			// The configured DTLS easing time constant (ms), so the Stream card's
			// tooltip reflects -entertainment-smooth-tau, not just the default. A
			// negative flag value clamps to 0 (smoothing off), matching the streamer.
			smoothTauMs: int(max(0, opts.smoothTau) / time.Millisecond),
		}
		// Push a fresh snapshot promptly whenever the setup machine changes, so the
		// wizard tracks transitions without waiting for the ~1s snapshot tick (and so a
		// reachability flip in step 3/5 reaches the browser immediately).
		setup.setOnChange(func() { uiHub.SetSnapshot(webui.BuildSnapshot(src)) })
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

	// Count each inbound REST control call (per-light state PUT + group-action PUT) for the
	// UI "Received" card's REST reading. Wired unconditionally — REST control drives the
	// lights in plain REST mode and as the entertainment fallback alike.
	clip.OnRESTControl = restRecvStats.Mark

	// entStreamer is hoisted to function scope so the shutdown path can release
	// relume-tv's own entertainment stream on the Pro synchronously (Phase D), rather
	// than racing the receiver's async OnStreamStop against process exit.
	var entStreamer *entertainment.ProStreamer

	if entertainmentMode {
		// Entertainment mode: run the real DTLS receiver on :2100. It decrypts the
		// TV's stream (PSK = the clientkey relume-tv minted at pairing) and decodes the
		// HueStream frames.
		recv := entertainment.NewReceiver(ip, cfg.PSKForUser, log)
		// Count stream frames as activity so the idle-off monitor doesn't turn the
		// lights off mid-stream (the TV streams via DTLS, not REST writes, here), and
		// record each frame's arrival so the UI can show the live frame rate.
		recv.OnActivity = func() {
			clip.MarkActivity()
			frameStats.Mark()
		}
		// Record the incoming stream's per-window brightness jump; paired with the
		// streamer's sent-side jump (below) it yields the jitter-reduction metric.
		recv.OnWindowStats = func(briJump, _ uint32) { jitterStats.setInput(briJump) }

		// Phase C: relume-tv opens its OWN entertainment stream to the Pro over DTLS and
		// re-encodes the decoded TV frames at full rate, avoiding the per-light REST writes
		// that overflow the Pro's command queue (503). The streamer auto-falls back to the
		// REST forward (Phase B) if DTLS cannot establish — so it is created unconditionally
		// (its fallback sink IS clip.ForwardLight). It resolves its Pro target LIVE from
		// cfg.GetPro() on each establish, so a pairing completed by autoPairPro or an IP
		// change followed by proWatcher takes effect on the next stream without rebuilding it.
		streamer := entertainment.NewProStreamer(nil, "", "", nil, clip.ForwardLight, log)
		streamer.SetProResolver(func() (entertainment.ProClient, string, string, []byte, bool) {
			p := cfg.GetPro()
			if p == nil || p.ClientKey == "" {
				return nil, "", "", nil, false
			}
			clientKey, err := hex.DecodeString(p.ClientKey)
			if err != nil {
				return nil, "", "", nil, false
			}
			return bridgepro.New(p), p.Host, p.AppKey, clientKey, true
		})
		// Easing time constant for the DTLS send path (configurable; 0 = off).
		streamer.SetSmoothTau(opts.smoothTau)
		// Surface the live DTLS-passthrough colours to the web UI (the REST path is
		// covered by the provider's OnColor via the fallback sink).
		streamer.OnColor = liveColors.SetStates
		// Record each frame sent to the Pro over DTLS so the UI can show the live
		// outgoing send rate (the 50 Hz counterpart to the TV's ~25 Hz input).
		streamer.OnSend = proSendStats.Mark
		// Record the smoothed sent stream's per-window brightness jump; the gap
		// below the receiver's input jump is how much the easing cut the flicker.
		streamer.OnWindowStats = func(briJump, _ uint32) { jitterStats.setSent(briJump) }
		// Honor the TV's group membership: when the TV declares which lights belong to
		// its Ambilight zone (POST/PUT /groups), restrict the Pro config to that
		// subset so lights in other rooms are never driven. Also filters the REST fallback.
		clip.OnGroupMembers = streamer.SetRequestedMembers
		// Persist/reuse relume-tv's own entertainment_configuration across restarts
		// instead of re-finding it each stream (Phase D).
		streamer.SetConfigStore(cfg.LoadEntConfigID, func(id string) {
			if err := cfg.SaveEntConfigID(id); err != nil {
				log.Warn("persisting relume-tv entertainment config id", "err", err)
			}
		})
		entStreamer = streamer
		// The TV opening its DTLS stream cancels the activation-fallback watchdog
		// (clip) and establishes the Pro stream (streamer). When no Pro is paired yet the
		// streamer stays on the REST fallback (clip.ForwardLight) until pairing completes.
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
		go func() {
			if err := recv.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("entertainment receiver", "err", err)
			}
		}()
	}

	// Detect the TV going silent (switched off / control session broke) and turn
	// the lights off — the TV sends no off signal, it just stops writing. Disabled
	// when the timeout is 0.
	if sc.idleOff > 0 {
		log.Info("idle-off monitor active", "timeout", sc.idleOff.String())
		go monitorIdle(ctx, clip, cfg, controlled, sc.idleOff, log)
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
	// Release relume-tv's own entertainment stream on the Pro before turning the lights
	// off, so the Pro area is deactivated (StopStream uses the client's own timeout, not
	// the cancelled ctx) and never leaks past process exit (Phase D).
	stopEntertainment(entStreamer)
	// Stop accepting TV writes first (above), then turn the driven lights off so they
	// don't stay frozen on their last Ambilight color across the restart. GetPro reads
	// the current pairing under the mutex (watchPro may have reconnected it to a new IP
	// since startup).
	if shutPro := cfg.GetPro(); shutPro != nil {
		turnOffBounded(bridgepro.New(shutPro), log, inZoneUUIDs(clip, controlled.Current()), shutdownTurnOffBudget)
	}
	return nil
}

// shutdownTurnOffBudget caps the total time the shutdown turn-off may take.
// TurnOffControlled runs synchronously and each light write blocks on the Hue Bridge
// Pro's HTTP timeout; if the Pro is unreachable at shutdown, an unbounded turn-off would
// stall well past the container stop grace (then get SIGKILLed). This bounds it so
// the process exits promptly.
const shutdownTurnOffBudget = 3 * time.Second

// turnOffBounded turns the driven lights off with a hard total deadline: it returns
// after the turn-off completes or after max elapses (whichever first). On timeout the
// goroutine is abandoned — the process is exiting anyway.
func turnOffBounded(client *bridgepro.Client, log *slog.Logger, ids []string, max time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridge.TurnOffControlled(client, log, "shutdown", ids)
	}()
	select {
	case <-done:
	case <-time.After(max):
		log.Warn("shutdown turn-off timed out (hue bridge pro unreachable?) — exiting anyway", "after", max.String())
	}
}

// zoneMembership is the slice of the clipv1 server inZoneUUIDs needs: resolve a Pro
// UUID to its v1 id and ask whether that id is in the TV's current Ambilight zone.
// *clipv1.Server satisfies it; an interface keeps the filter testable in isolation.
type zoneMembership interface {
	V1ForUUID(uuid string) (string, bool)
	AllowsMember(v1id uint16) bool
}

// inZoneUUIDs filters a turn-off target (Hue Bridge Pro light UUIDs) down to the lights
// still in the TV's current Ambilight zone, so the restart/idle turn-off never touches
// an off-zone light — even one that lingers in the ControlledSet from before the zone
// shrank. A UUID is dropped only when its v1 id resolves AND is positively outside the
// zone; an unresolved UUID is kept, mirroring AllowsMember's defensive "allow when
// unknown" stance so the turn-off is never silently reduced to nothing. With no zone
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

// newProvider builds the Hue Bridge Pro light provider and wires it to feed the
// sliding-window ControlledSet with each light the TV drives, so the restart/idle
// turn-off touches only the bulbs the TV is currently driving — never the
// rest of the home.
func newProvider(client *bridgepro.Client, controlled *bridge.ControlledSet, live *liveColors, stats *proStats, log *slog.Logger) *bridge.LightProvider {
	p := bridge.NewLightProvider(client, log)
	p.OnControlled = controlled.Seen
	p.OnColor = live.SetState
	// Record each successful REST write to the Pro so the UI can show the live
	// outgoing write rate when relume-tv drives the Pro over REST (no DTLS stream).
	p.OnForward = stats.writes.Mark
	// Backpressure signals for the UI: coalesced (healthy drops/s the optimistic
	// path spared the Pro) and forward errors (the real failure signal, cumulative).
	p.OnCoalesce = stats.coalesces.Mark
	p.OnForwardErr = stats.markForwardErr
	return p
}

func shutdownHTTP(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// monitorIdle watches the TV's Ambilight write activity and, once it has gone
// silent for idleTimeout after having been active, turns the Hue Bridge Pro lights
// off (bridge.TurnOffControlled). The TV sends no explicit off signal — it just
// stops writing — so this inactivity timeout stands in for it. It fires once per
// active→idle transition and re-arms when the TV resumes writing. The turn-off is a
// no-op while no Pro is paired or the Pro is unreachable.
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
			// re-pair concurrently. nil while no Pro is paired: nothing to turn off.
			if pro := cfg.GetPro(); pro != nil {
				log.Info("ambilight idle: turning the ambilight lights off", "idle_for", now.Sub(lastSeen).Round(time.Second).String())
				bridge.TurnOffControlled(bridgepro.New(pro), log, "idle-off", inZoneUUIDs(clip, controlled.Current()))
			}
		}
	}
}

// idleShouldFire reports whether the idle-off should fire this tick: the TV
// has been active at least once (lastSeen non-zero), has now been silent for the
// timeout, and it has not already fired for this active→idle transition.
func idleShouldFire(now, lastSeen time.Time, fired bool, idleTimeout time.Duration) bool {
	return !fired && !lastSeen.IsZero() && now.Sub(lastSeen) >= idleTimeout
}

// setupWatcherInterval is the fast Pro health-check cadence used during setup so the
// power-off (step 3) and power-on (step 5) transitions feel responsive in the wizard.
const setupWatcherInterval = 4 * time.Second

// autoPairPro pairs relume-tv with the Hue Bridge Pro in the background, independently of
// the TV side. It discovers the Pro via local mDNS (the only discovery path),
// selects the first bridge that is really a Pro (modelid BSB003), pins the leaf
// certificate, then polls until the user taps the Pro's physical link button (the one
// step that cannot be automated). It reports the discovery preconditions to the setup
// wizard, and on success persists the credentials, hot-loads the light backend and
// starts the setup reachability watcher (steps 3 & 5).
func autoPairPro(ctx context.Context, cfg *config.Config, clip *clipv1.Server, controlled *bridge.ControlledSet, live *liveColors, stats *proStats, setup *setupStatus, skipTLS bool, log *slog.Logger) {
	// Browse mDNS for Hue bridges, excluding relume-tv's own announcement (it advertises
	// itself as a Hue bridge to the TV under this same bridge id).
	discover := func() ([]bridgepro.DiscoveredBridge, error) {
		return bridgepro.Discover(cfg.Identity.BridgeID())
	}
	var host, discoveryID string
	for host == "" {
		h, id, modelID, derr := selectProForPairing(discover, bridgepro.FetchModelID, log)
		switch {
		case derr == nil && h != "":
			host, discoveryID = h, id
			log.Info("hue bridge pro discovered", "host", host, "modelid", modelID, "via", "mDNS (_hue._tcp.local)")
			setup.setPrecond(true, host, true, "")
		case errors.Is(derr, ErrNoProBridge):
			// (b) a bridge was found but it is not a Pro — surface the actual modelid.
			msg := "A Hue bridge was found, but it is not a Hue Bridge Pro (BSB003). relume-tv requires a Hue Bridge Pro."
			log.Warn("hue bridge pro not paired yet: discovered bridge is not a Hue Bridge Pro; retrying", "err", derr)
			setup.setPrecond(true, "", false, msg)
		case errors.Is(derr, ErrProModelUnknown):
			// (c)-ish a bridge was found but unreachable to confirm its modelid.
			log.Warn("hue bridge pro not paired yet: a bridge was found but is unreachable to confirm it is a Hue Bridge Pro; retrying", "err", derr)
			setup.setPrecond(true, "", false, "A bridge was found but could not be reached to confirm it is a Hue Bridge Pro. Is it powered on?")
		case derr != nil:
			// (c) the mDNS discovery subsystem itself failed (no multicast interface, etc.).
			log.Warn("hue bridge pro not paired yet: mDNS discovery unavailable — turn the hue bridge pro on and check this host is on the same network; retrying", "err", derr)
			setup.setPrecond(false, "", false, "Can't run local (mDNS) discovery. Turn your Hue Bridge Pro on and make sure this host is on the same network.")
		default:
			// (a) discovery worked but found no bridge → the Hue Bridge Pro is off/absent.
			log.Warn("hue bridge pro not paired yet: no bridge found via mDNS — turn the hue bridge pro on; retrying")
			setup.setPrecond(true, "", false, "No Hue Bridge Pro found yet — please turn it on and connect it to the same network.")
		}
		if host != "" {
			break
		}
		// mDNS is local and unthrottled, so we can retry briskly while the user powers on
		// or connects the bridge.
		if !sleepCtx(ctx, 10*time.Second) {
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
	paired, perr := bridgepro.NewPairer("relume-tv#"+hostname()).
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
	// Advance the wizard past step 1 (proPaired) promptly.
	setup.recomputeNow()
	// Start the setup reachability watcher: it keeps the Pro reachable (re-discover →
	// reconnect, so a DHCP IP change after the step-5 power-cycle is followed) AND feeds
	// setup.setReachable each tick so steps 3 and 5 advance. Faster cadence than the
	// steady-state watcher so the power-cycle transitions feel responsive.
	w := newProWatcher(cfg, clip, controlled, live, stats, skipTLS, log)
	w.interval = setupWatcherInterval
	w.onReachable = setup.setReachable
	go w.run(ctx)
}

// reconnectProConfig builds the Hue Bridge Pro config for a reconnect: it keeps the
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

// runAvahiService prints an Avahi static service file with which a Linux host
// running avahi-daemon announces the _hue._tcp service. Needed when avahi
// occupies port 5353 and relume-tv's built-in mDNS announcer therefore can't bind:
//
//	relume-tv avahi-service > /etc/avahi/services/relume-tv-hue.service
func runAvahiService(args []string) error {
	fs := flag.NewFlagSet("avahi-service", flag.ExitOnError)
	cfgPath := fs.String("config", "relume-tv.json", "path to the configuration file")
	port := fs.Int("http-port", 80, "advertised port (must match the serve http-port)")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// With deferred persistence, a config-less host produces a throwaway in-memory
	// identity that does NOT match the serial a later committed setup will write. Warn
	// so the operator regenerates this file after completing the setup (otherwise the
	// avahi instance name won't match the running bridge).
	if cfg.FirstRun() {
		fmt.Fprintln(os.Stderr, "warning: no config file yet — this avahi service uses a temporary identity. "+
			"Complete the setup first (run serve, finish the wizard), then re-run avahi-service to emit the committed identity.")
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
