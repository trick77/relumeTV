// Package mdns actively announces relume-tv as a Hue bridge via mDNS/Bonjour
// (_hue._tcp.local.). Modern Philips TVs (and the Hue Bridge Pro itself) find the
// bridge primarily this way; they passively listen for the announcement and
// often make no request of their own. The format follows hass-emulated-hue,
// which the Ambilight TV is known to discover: instance name
// "Philips Hue - XXXXXX", TXT with bridgeid and modelid=BSB002.
package mdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/trick77/relume-tv/internal/config"
	"github.com/trick77/relume-tv/internal/netutil"
)

const (
	service = "_hue._tcp"
	domain  = "local."
)

// Announcer keeps the mDNS registration alive.
type Announcer struct {
	id    config.Identity
	advIP string
	port  int
	log   *slog.Logger
	// BurstDuration enables a diagnostic re-announcement burst after startup.
	// Defaults to disabled.
	BurstDuration time.Duration
	// BurstInterval is the interval used during the diagnostic burst.
	BurstInterval time.Duration
}

type serviceSpec struct {
	instance string
	service  string
	domain   string
	host     string
	txt      []string
}

// New creates an Announcer. port is the advertised SRV port (usually the
// HTTP port of the emulated bridge).
func New(id config.Identity, advIP string, port int, log *slog.Logger) *Announcer {
	return &Announcer{id: id, advIP: advIP, port: port, log: log}
}

// Run registers the service and keeps it announced until ctx is cancelled.
func (a *Announcer) Run(ctx context.Context) error {
	spec := a.serviceSpec()

	var ifaces []net.Interface
	if iface, err := netutil.InterfaceForIP(a.advIP); err != nil {
		a.log.Warn("mdns: interface for advertise IP not found, using all", "err", err)
	} else {
		ifaces = []net.Interface{*iface}
	}

	// A real Gen-2 Hue bridge is IPv4-only. RegisterProxy with an explicit IPv4
	// list announces only an A record (no AAAA) — relying on the host's interface
	// addresses (zeroconf.Register) would also publish IPv6 AAAA records, which a
	// real bridge never has and which some TVs reject or mis-handle.
	register := func() (*zeroconf.Server, error) {
		return zeroconf.RegisterProxy(spec.instance, spec.service, spec.domain, a.port, spec.host, []string{a.advIP}, spec.txt, ifaces)
	}

	server, err := register()
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}
	// Deliberately NOT calling server.Shutdown() on exit. zeroconf's Shutdown
	// multicasts an mDNS "goodbye" (records with TTL 0); the Ambilight TV caches
	// the _hue._tcp answer, so a goodbye evicts relume-tv from its bridge list. With a
	// powered-on Hue Bridge Pro on the LAN the TV then will NOT re-list relume-tv on
	// re-discovery (it prefers the Pro/BSB003), so a plain restart would drop the
	// bridge from the Ambilight list until the Pro is power-cycled. Letting the
	// process exit closes the socket WITHOUT a goodbye, so the TV keeps relume-tv
	// cached across restarts; the next start simply re-announces. This is the same
	// "never emit a goodbye" reasoning as the no-periodic-re-register note below —
	// the shutdown path was the remaining goodbye source.
	_ = server
	a.log.Info("mdns: announced as hue bridge",
		"instance", spec.instance, "host", spec.host+"."+spec.domain, "ip", a.advIP, "port", a.port, "bridgeid", a.id.BridgeID())

	// Register exactly once and keep the responder alive; grandcat/zeroconf answers
	// the TV's active _hue._tcp queries from here on.
	//
	// We deliberately do NOT periodically re-register. Re-registration goes through
	// Server.Shutdown(), which multicasts an mDNS "goodbye" (records with TTL 0)
	// before re-announcing. The Ambilight TV actively queries _hue._tcp (confirmed
	// by packet capture) and caches the answer, so a goodbye evicts relume-tv from the
	// TV's bridge list — the bridge flickers out mid-discovery and never appears.
	// The confirmed-working ha-hue-entertainment emulator also registers exactly
	// once. This is why relume-tv served an identical descriptor yet was never listed.
	if a.BurstDuration > 0 {
		// The startup discovery burst is handled by the SSDP responder. mDNS needs
		// no burst: the TV queries actively, and a real re-announce here would only
		// emit harmful goodbye packets (see above).
		a.log.Info("mdns: registered once; discovery burst handled via SSDP (no mDNS goodbye)")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *Announcer) serviceSpec() serviceSpec {
	bridgeID := a.id.BridgeID()
	host := a.id.Serial
	return serviceSpec{
		instance: "Philips Hue - " + bridgeID[len(bridgeID)-6:],
		service:  service,
		domain:   domain,
		// Unique, bridge-like hostname for the SRV target / A record so it never
		// collides with the host's own mDNS name (e.g. nas.local).
		host: host,
		txt: []string{
			"bridgeid=" + bridgeID,
			"modelid=BSB002",
		},
	}
}
