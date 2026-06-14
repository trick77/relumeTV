// Command relume is a software bridge that connects a Philips Ambilight TV to
// a Hue Bridge Pro by presenting itself to the TV as a Gen-2 bridge and
// forwarding commands to the Pro.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/trick77/relume/internal/bridge"
	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/clipv1"
	"github.com/trick77/relume/internal/config"
	"github.com/trick77/relume/internal/diag"
	"github.com/trick77/relume/internal/mdns"
	"github.com/trick77/relume/internal/ssdp"
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
	configPath               string
	httpPort                 int
	advertiseIP              string
	debug                    bool
	tvIP                     string
	discoveryBurstDuration   time.Duration
	discoveryBurstInterval   time.Duration
	identityProfile          string
	descriptionProfile       string
	ssdpMediaServerAlias     bool
	ssdpMediaServerBasicBody bool
	ssdpDescriptorVariants   bool
	disableSSDP              bool
	bridgeIP                 string
	skipTLS                  bool
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
	identityProfile := fs.String("identity-profile", "", "experimental identity profile: empty/default, ambilight, or hass")
	descriptionProfile := fs.String("description-profile", "", "experimental description.xml profile: empty/default or ambilight-reference")
	ssdpMediaServerAlias := fs.Bool("ssdp-media-server-alias", false, "also advertise/respond as UPnP MediaServer:1 for Philips TV discovery experiments")
	ssdpMediaServerBasicBody := fs.Bool("ssdp-media-server-basic-body", false, "serve a Hue Basic descriptor body from the MediaServer alias URL")
	ssdpDescriptorVariants := fs.Bool("ssdp-descriptor-variants", false, "also advertise query-scoped descriptor variants for Philips TV discovery experiments")
	disableSSDP := fs.Bool("disable-ssdp", false, "do not run the SSDP responder (mDNS-only, like ha-hue-entertainment) — diagnostic")
	bridgeIP := fs.String("bridge-ip", "", "Bridge Pro IP for auto-pairing (empty = cloud discovery)")
	skipTLS := fs.Bool("skip-tls-verify", false, "skip TLS verification to the Bridge Pro (instead of cert pinning)")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	return serveOptions{
		configPath:               *cfgPath,
		httpPort:                 *httpPort,
		advertiseIP:              *advIP,
		debug:                    *debug,
		tvIP:                     *tvIP,
		discoveryBurstDuration:   *burstDuration,
		discoveryBurstInterval:   *burstInterval,
		identityProfile:          *identityProfile,
		descriptionProfile:       *descriptionProfile,
		ssdpMediaServerAlias:     *ssdpMediaServerAlias,
		ssdpMediaServerBasicBody: *ssdpMediaServerBasicBody,
		ssdpDescriptorVariants:   *ssdpDescriptorVariants,
		disableSSDP:              *disableSSDP,
		bridgeIP:                 *bridgeIP,
		skipTLS:                  *skipTLS,
	}, nil
}

func runServe(args []string, log *slog.Logger) error {
	opts, err := parseServeOptions(args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	ip := opts.advertiseIP
	if ip == "" {
		ip, err = outboundIP()
		if err != nil {
			return fmt.Errorf("auto-detect advertise-ip: %w (use -advertise-ip)", err)
		}
	}
	log.Info("relume", "version", version)
	log.Info("identity", "serial", cfg.Identity.Serial, "bridgeid", cfg.Identity.BridgeID(), "advertise", ip)

	clip := clipv1.New(cfg, ip, opts.httpPort, log)
	clip.Debug = opts.debug
	clip.TVIP = opts.tvIP
	clip.IdentityProfile = opts.identityProfile
	clip.DescriptionProfile = opts.descriptionProfile
	clip.MediaServerAlias = opts.ssdpMediaServerAlias
	clip.MediaServerBasicBody = opts.ssdpMediaServerBasicBody
	if cfg.Pro != nil {
		client := bridgepro.New(cfg.Pro)
		clip.SetLightProvider(bridge.NewLightProvider(client))
		log.Info("bridge pro paired", "host", cfg.Pro.Host)
	}
	var responder *ssdp.Responder
	if opts.disableSSDP {
		log.Info("ssdp: disabled (mDNS-only mode)")
	} else {
		responder = ssdp.New(cfg.Identity, ip, opts.httpPort, log)
		responder.Debug = opts.debug
		responder.BurstDuration = opts.discoveryBurstDuration
		responder.BurstInterval = opts.discoveryBurstInterval
		responder.IdentityProfile = opts.identityProfile
		responder.MediaServerAlias = opts.ssdpMediaServerAlias
		responder.DescriptorVariants = opts.ssdpDescriptorVariants
	}
	announcer := mdns.New(cfg.Identity, ip, opts.httpPort, log)
	announcer.IdentityProfile = opts.identityProfile
	announcer.BurstDuration = opts.discoveryBurstDuration
	announcer.BurstInterval = opts.discoveryBurstInterval

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Pair the Bridge Pro backend in the background, independently of the TV: the
	// TV can discover/pair relume before the Pro is paired; relume just returns an
	// empty light list until the Pro pairing completes, then hot-loads the lights.
	if cfg.Pro == nil {
		log.Warn("no bridge pro paired yet – auto-pairing in background; TAP the Bridge Pro link button")
		go autoPairPro(ctx, cfg, clip, opts.bridgeIP, opts.skipTLS, log)
	} else {
		// Keep the already-paired Pro reachable across reboots / IP changes.
		go watchPro(ctx, cfg, clip, opts.bridgeIP, opts.skipTLS, log)
	}

	// Summarize the high-frequency Ambilight light-state writes periodically
	// instead of logging every single request.
	go clip.LogActivitySummary(ctx, 30*time.Second)

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
		return err
	}
	shutdownHTTP(httpSrv)
	return nil
}

func shutdownHTTP(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// autoPairPro pairs relume with the Bridge Pro in the background, independently of
// the TV side. It discovers the Pro (cloud, unless bridgeIP is given), pins the
// leaf certificate, then polls until the user taps the Pro's physical link button
// (the one step that cannot be automated). On success it persists the credentials
// and hot-loads the light backend so the already-paired TV starts seeing lights.
func autoPairPro(ctx context.Context, cfg *config.Config, clip *clipv1.Server, bridgeIP string, skipTLS bool, log *slog.Logger) {
	host := bridgeIP
	for host == "" {
		bridges, derr := bridgepro.Discover()
		if derr == nil && len(bridges) > 0 {
			host = bridges[0].InternalIPAddress
			log.Info("bridge pro discovered", "host", host)
			break
		}
		log.Warn("bridge pro not paired yet: not found via cloud discovery — power the Bridge Pro on, or pass -bridge-ip; retrying", "err", derr)
		if !sleepCtx(ctx, 15*time.Second) {
			return
		}
	}

	pro := &config.BridgePro{Host: host, SkipTLSVerify: skipTLS}
	for !skipTLS && pro.CertSHA256 == "" {
		fp, ferr := bridgepro.FetchLeafFingerprint(host)
		if ferr == nil {
			pro.CertSHA256 = fp
			log.Info("bridge pro certificate pinned", "sha256", fp)
			break
		}
		log.Warn("bridge pro not paired yet: cannot reach it to pin its certificate — power the Bridge Pro on; retrying", "host", host, "err", ferr)
		if !sleepCtx(ctx, 15*time.Second) {
			return
		}
	}

	httpClient := bridgepro.HTTPClientFor(pro)
	log.Info("waiting for the Bridge Pro link button — TAP it now", "host", host)
	for attempts := 0; ; attempts++ {
		res, perr := bridgepro.Pair(httpClient, host, "relume#"+hostname())
		if perr == nil {
			pro.AppKey = res.AppKey
			pro.ClientKey = res.ClientKey
			if serr := cfg.SetPro(pro); serr != nil {
				log.Error("persisting bridge pro pairing", "err", serr)
				return
			}
			client := bridgepro.New(pro)
			clip.SetLightProvider(bridge.NewLightProvider(client))
			log.Info("bridge pro paired (auto)", "host", host)
			if lights, lerr := client.Lights(); lerr == nil {
				log.Info("bridge pro lights available", "count", len(lights))
			}
			return
		}
		if attempts%6 == 0 {
			log.Info("still waiting for the Bridge Pro link button — TAP it", "host", host)
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return
		}
	}
}

// watchPro keeps the already-paired Bridge Pro reachable. It health-checks
// periodically and, on failure, re-discovers the Pro's current IP (cloud or
// -bridge-ip), re-pins its certificate and hot-swaps the light provider — all
// without a new button press, since the stored appKey/clientKey stay valid
// across reboots and DHCP IP changes.
func watchPro(ctx context.Context, cfg *config.Config, clip *clipv1.Server, bridgeIP string, skipTLS bool, log *slog.Logger) {
	const checkInterval = 60 * time.Second
	pro := cfg.Pro
	if pro == nil {
		return
	}
	for sleepCtx(ctx, checkInterval) {
		if _, err := bridgepro.New(pro).Lights(); err == nil {
			continue // still reachable
		}
		log.Warn("bridge pro unreachable; attempting to reconnect", "host", pro.Host)

		host := bridgeIP
		if host == "" {
			if bridges, derr := bridgepro.Discover(); derr == nil && len(bridges) > 0 {
				host = bridges[0].InternalIPAddress
			}
		}
		if host == "" {
			log.Warn("bridge pro reconnect: not found via discovery; will retry")
			continue
		}

		certSHA := pro.CertSHA256
		if !skipTLS && !pro.SkipTLSVerify {
			fp, ferr := bridgepro.FetchLeafFingerprint(host)
			if ferr != nil {
				log.Warn("bridge pro reconnect: cert fetch failed; will retry", "host", host, "err", ferr)
				continue
			}
			certSHA = fp
		}

		updated := reconnectProConfig(pro, host, certSHA, skipTLS)
		if _, err := bridgepro.New(updated).Lights(); err != nil {
			log.Warn("bridge pro reconnect: still unreachable", "host", host, "err", err)
			continue
		}
		if serr := cfg.SetPro(updated); serr != nil {
			log.Error("persisting reconnected bridge pro", "err", serr)
			continue
		}
		clip.SetLightProvider(bridge.NewLightProvider(bridgepro.New(updated)))
		pro = updated
		log.Info("bridge pro reconnected", "host", host)
	}
}

// reconnectProConfig builds the Bridge Pro config for a reconnect: it keeps the
// existing credentials (appKey/clientKey — valid across reboots and IP changes,
// so no re-pairing) and refreshes only the host and pinned certificate.
func reconnectProConfig(old *config.BridgePro, host, certSHA256 string, skipTLS bool) *config.BridgePro {
	return &config.BridgePro{
		Host:          host,
		AppKey:        old.AppKey,
		ClientKey:     old.ClientKey,
		CertSHA256:    certSHA256,
		SkipTLSVerify: skipTLS || old.SkipTLSVerify,
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

	host := *bridgeIP
	if host == "" {
		bridges, derr := bridgepro.Discover()
		if derr != nil || len(bridges) == 0 {
			return fmt.Errorf("no bridge found; please specify -bridge-ip (discover: %v)", derr)
		}
		host = bridges[0].InternalIPAddress
		fmt.Printf("Bridge found via cloud discovery: %s\n", host)
	}

	pro := &config.BridgePro{Host: host, SkipTLSVerify: *skipTLS}
	if !*skipTLS {
		fp, ferr := bridgepro.FetchLeafFingerprint(host)
		if ferr != nil {
			return fmt.Errorf("pin certificate: %w", ferr)
		}
		pro.CertSHA256 = fp
		log.Info("certificate pinned", "sha256", fp)
	}

	httpClient := bridgepro.HTTPClientFor(pro)
	fmt.Printf("\n>>> Now press the link button on the Hue Bridge Pro (%s) <<<\n\n", host)

	deadline := time.Now().Add(*timeout)
	var res *bridgepro.PairResult
	for time.Now().Before(deadline) {
		res, err = bridgepro.Pair(httpClient, host, "relume#"+hostname())
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
		fmt.Print(".")
	}
	fmt.Println()
	if res == nil {
		return fmt.Errorf("pairing failed (press the link button in time): %w", err)
	}

	pro.AppKey = res.AppKey
	pro.ClientKey = res.ClientKey
	if err := cfg.SetPro(pro); err != nil {
		return err
	}
	fmt.Println("Pairing successful, app key saved.")

	// List lights as confirmation.
	client := bridgepro.New(pro)
	lights, lerr := client.Lights()
	if lerr != nil {
		fmt.Printf("Note: lights could not be read: %v\n", lerr)
		return nil
	}
	fmt.Printf("%d lights found:\n", len(lights))
	for _, l := range lights {
		fmt.Printf("  - %s (%s)\n", l.Metadata.Name, l.ID)
	}
	return nil
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
