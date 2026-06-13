// Package mdns actively announces relume as a Hue bridge via mDNS/Bonjour
// (_hue._tcp.local.). Modern Philips TVs (and the Bridge Pro itself) find the
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
	"github.com/trick77/relume/internal/config"
)

const (
	service = "_hue._tcp"
	domain  = "local."
	// reannounceEvery re-publishes the mDNS record periodically. grandcat/zeroconf
	// only announces once at registration and otherwise just answers active
	// queries; the Ambilight TV listens passively and never queries _hue._tcp, so
	// without this it only ever hears us in the brief window right after startup.
	reannounceEvery = 30 * time.Second
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
	if iface, err := interfaceForIP(a.advIP); err != nil {
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
	a.log.Info("mdns: announced as hue bridge",
		"instance", spec.instance, "host", spec.host+"."+spec.domain, "ip", a.advIP, "port", a.port, "bridgeid", a.id.BridgeID())

	// Re-announce periodically so a passively-listening TV hears us regardless of
	// when its search starts. grandcat/zeroconf has no real conflict resolution,
	// so re-registering never renames the instance.
	t := time.NewTicker(reannounceEvery)
	defer t.Stop()
	var burstC <-chan time.Time
	var burstT *time.Ticker
	var burstUntil time.Time
	if a.BurstDuration > 0 {
		interval := a.BurstInterval
		if interval <= 0 {
			interval = time.Second
		}
		burstT = time.NewTicker(interval)
		defer burstT.Stop()
		burstC = burstT.C
		burstUntil = time.Now().Add(a.BurstDuration)
		a.log.Info("mdns: discovery burst started", "duration", a.BurstDuration, "interval", interval)
	}
	for {
		select {
		case <-ctx.Done():
			server.Shutdown()
			return ctx.Err()
		case <-burstC:
			if time.Now().After(burstUntil) {
				burstT.Stop()
				burstC = nil
				a.log.Info("mdns: discovery burst finished")
				continue
			}
			s, rerr := reRegister(server, register)
			if rerr != nil {
				a.log.Warn("mdns: burst re-announce failed", "err", rerr)
				continue
			}
			server = s
			a.log.Info("mdns: burst re-announced as hue bridge", "instance", spec.instance, "ip", a.advIP, "port", a.port)
		case <-t.C:
			s, rerr := reRegister(server, register)
			if rerr != nil {
				a.log.Warn("mdns: re-announce failed", "err", rerr)
				continue
			}
			server = s
		}
	}
}

func (a *Announcer) serviceSpec() serviceSpec {
	bridgeID := a.id.BridgeID()
	return serviceSpec{
		instance: "Philips Hue - " + bridgeID[len(bridgeID)-6:],
		service:  service,
		domain:   domain,
		// Unique, bridge-like hostname for the SRV target / A record so it never
		// collides with the host's own mDNS name (e.g. nas.local).
		host: a.id.Serial,
		txt: []string{
			"bridgeid=" + bridgeID,
			"modelid=BSB002",
		},
	}
}

func reRegister(current *zeroconf.Server, register func() (*zeroconf.Server, error)) (*zeroconf.Server, error) {
	current.Shutdown()
	return register()
}

// interfaceForIP returns the multicast-capable interface that carries the given IP.
func interfaceForIP(ip string) (*net.Interface, error) {
	target := net.ParseIP(ip)
	if target == nil {
		return nil, fmt.Errorf("invalid IP %q", ip)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagMulticast == 0 || ifaces[i].Flags&net.FlagUp == 0 {
			continue
		}
		addrs, aerr := ifaces[i].Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(target) {
				return &ifaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no multicast-capable interface with IP %s", ip)
}
