// Command relume ist eine Software-Bridge, die einen Philips Ambilight-TV mit
// einer Hue Bridge Pro verbindet, indem sie sich gegenüber dem TV als Gen-2-Bridge
// ausgibt und Befehle an die Pro weiterreicht.
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

// version wird beim Build per -ldflags "-X main.version=..." gesetzt (CI).
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
			log.Error("serve beendet", "err", err)
			os.Exit(1)
		}
	case "link":
		if err := runLink(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
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
		fmt.Fprintf(os.Stderr, "unbekannter Befehl %q\nVerfügbar: serve, setup, discover, link, avahi-service\n", cmd)
		os.Exit(2)
	}
}

func runServe(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "relume.json", "Pfad zur Konfigurationsdatei")
	httpPort := fs.Int("http-port", 80, "HTTP-Port der emulierten Bridge")
	advIP := fs.String("advertise-ip", "", "beworbene IP (leer = auto-detektieren)")
	debug := fs.Bool("debug", false, "ausführliche Diagnose: SSDP-/HTTP-Datagramme + mDNS-Observer")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	ip := *advIP
	if ip == "" {
		ip, err = outboundIP()
		if err != nil {
			return fmt.Errorf("advertise-ip auto-detektieren: %w (nutze -advertise-ip)", err)
		}
	}
	log.Info("relume", "version", version)
	log.Info("identität", "serial", cfg.Identity.Serial, "bridgeid", cfg.Identity.BridgeID(), "advertise", ip)

	clip := clipv1.New(cfg, ip, *httpPort, log)
	clip.Debug = *debug
	if cfg.Pro != nil {
		client := bridgepro.New(cfg.Pro)
		clip.SetLightProvider(bridge.NewLightProvider(client))
		log.Info("bridge pro gekoppelt", "host", cfg.Pro.Host)
	} else {
		log.Warn("keine bridge pro gekoppelt – erst 'relume setup' ausführen")
	}
	responder := ssdp.New(cfg.Identity, ip, *httpPort, log)
	responder.Debug = *debug
	announcer := mdns.New(cfg.Identity, ip, *httpPort, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := announcer.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("mdns-announcer", "err", err)
		}
	}()

	if *debug {
		obs := diag.NewMDNSObserver(ip, log)
		go func() {
			if err := obs.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("mdns-observer", "err", err)
			}
		}()
		log.Info("debug-modus aktiv: SSDP-/HTTP-Diagnose + mDNS-Observer")
	}

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *httpPort),
		Handler: clip.Handler(),
	}

	errc := make(chan error, 2)
	go func() {
		log.Info("http-server gestartet", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- fmt.Errorf("http: %w", err)
		}
	}()
	go func() {
		if err := responder.Run(ctx); err != nil && ctx.Err() == nil {
			errc <- fmt.Errorf("ssdp: %w", err)
		}
	}()

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

// runLink öffnet das Pairing-Fenster, indem es den laufenden serve-Prozess anstößt.
func runLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "Host des laufenden relume")
	port := fs.Int("http-port", 80, "HTTP-Port")
	_ = fs.Parse(args)

	url := fmt.Sprintf("http://%s:%d/link", *host, *port)
	resp, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		return fmt.Errorf("link-anfrage an %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("link-anfrage fehlgeschlagen: %s", resp.Status)
	}
	fmt.Println("Link-Button gedrückt – Pairing für 30s offen.")
	return nil
}

// runDiscover listet via Philips-Cloud gefundene Bridges im lokalen Netz.
func runDiscover() error {
	bridges, err := bridgepro.Discover()
	if err != nil {
		return err
	}
	if len(bridges) == 0 {
		fmt.Println("Keine Bridges gefunden (Cloud-Discovery). Nutze setup -bridge-ip <ip>.")
		return nil
	}
	fmt.Println("Gefundene Bridges:")
	for _, b := range bridges {
		fmt.Printf("  id=%s  ip=%s\n", b.ID, b.InternalIPAddress)
	}
	return nil
}

// runAvahiService gibt eine Avahi-Static-Service-Datei aus, mit der ein Linux-Host
// mit laufendem avahi-daemon den _hue._tcp-Dienst announct. Nötig, wenn avahi
// Port 5353 belegt und relumes eingebauter mDNS-Announcer deshalb nicht greift:
//   relume avahi-service > /etc/avahi/services/relume-hue.service
func runAvahiService(args []string) error {
	fs := flag.NewFlagSet("avahi-service", flag.ExitOnError)
	cfgPath := fs.String("config", "relume.json", "Pfad zur Konfigurationsdatei")
	port := fs.Int("http-port", 80, "beworbener Port (muss zum serve-http-port passen)")
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

// runSetup koppelt relume mit der echten Hue Bridge Pro: Zertifikat pinnen,
// Link-Button abwarten, App-Key + clientkey holen und persistieren.
func runSetup(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", "relume.json", "Pfad zur Konfigurationsdatei")
	bridgeIP := fs.String("bridge-ip", "", "IP der Hue Bridge Pro (leer = Cloud-Discovery)")
	skipTLS := fs.Bool("skip-tls-verify", false, "TLS-Prüfung gegen die Pro deaktivieren (statt Cert-Pinning)")
	timeout := fs.Duration("timeout", 60*time.Second, "wie lange auf den Link-Button gewartet wird")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	host := *bridgeIP
	if host == "" {
		bridges, derr := bridgepro.Discover()
		if derr != nil || len(bridges) == 0 {
			return fmt.Errorf("keine Bridge gefunden; bitte -bridge-ip angeben (discover: %v)", derr)
		}
		host = bridges[0].InternalIPAddress
		fmt.Printf("Bridge per Cloud-Discovery gefunden: %s\n", host)
	}

	pro := &config.BridgePro{Host: host, SkipTLSVerify: *skipTLS}
	if !*skipTLS {
		fp, ferr := bridgepro.FetchLeafFingerprint(host)
		if ferr != nil {
			return fmt.Errorf("zertifikat pinnen: %w", ferr)
		}
		pro.CertSHA256 = fp
		log.Info("zertifikat gepinnt", "sha256", fp)
	}

	httpClient := bridgepro.HTTPClientFor(pro)
	fmt.Printf("\n>>> Drücke jetzt den Link-Button an der Hue Bridge Pro (%s) <<<\n\n", host)

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
		return fmt.Errorf("kopplung fehlgeschlagen (Link-Button rechtzeitig drücken): %w", err)
	}

	pro.AppKey = res.AppKey
	pro.ClientKey = res.ClientKey
	if err := cfg.SetPro(pro); err != nil {
		return err
	}
	fmt.Println("Kopplung erfolgreich, App-Key gespeichert.")

	// Lampen zur Bestätigung auflisten.
	client := bridgepro.New(pro)
	lights, lerr := client.Lights()
	if lerr != nil {
		fmt.Printf("Hinweis: Lampen konnten nicht gelesen werden: %v\n", lerr)
		return nil
	}
	fmt.Printf("%d Lampen gefunden:\n", len(lights))
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

// outboundIP ermittelt die lokale IPv4, über die ausgehender Verkehr läuft.
func outboundIP() (string, error) {
	conn, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, kein echter Traffic
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
