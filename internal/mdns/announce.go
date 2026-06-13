// Package mdns kündigt relume aktiv per mDNS/Bonjour als Hue-Bridge an
// (_hue._tcp.local.). Moderne Philips-TVs (und die Bridge Pro selbst) finden die
// Bridge primär so; sie lauschen passiv auf das Announcement und stellen oft gar
// keine eigene Anfrage. Das Format folgt dem nachweislich vom Ambilight-TV
// gefundenen hass-emulated-hue: Instanzname "Philips Hue - XXXXXX", TXT mit
// bridgeid und modelid=BSB002.
package mdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/grandcat/zeroconf"
	"github.com/trick77/relume/internal/config"
)

const (
	service = "_hue._tcp"
	domain  = "local."
)

// Announcer hält die mDNS-Registrierung am Leben.
type Announcer struct {
	id    config.Identity
	advIP string
	port  int
	log   *slog.Logger
}

// New erstellt einen Announcer. port ist der beworbene SRV-Port (i.d.R. der
// HTTP-Port der emulierten Bridge).
func New(id config.Identity, advIP string, port int, log *slog.Logger) *Announcer {
	return &Announcer{id: id, advIP: advIP, port: port, log: log}
}

// Run registriert den Service und hält ihn bis ctx beendet wird.
func (a *Announcer) Run(ctx context.Context) error {
	bridgeID := a.id.BridgeID()
	instance := "Philips Hue - " + bridgeID[len(bridgeID)-6:]
	txt := []string{
		"bridgeid=" + bridgeID,
		"modelid=BSB002",
	}

	var ifaces []net.Interface
	if iface, err := interfaceForIP(a.advIP); err != nil {
		a.log.Warn("mdns: interface zur advertise-ip nicht gefunden, nutze alle", "err", err)
	} else {
		ifaces = []net.Interface{*iface}
	}

	server, err := zeroconf.Register(instance, service, domain, a.port, txt, ifaces)
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}
	defer server.Shutdown()

	a.log.Info("mdns: als hue-bridge announct",
		"instance", instance, "service", service, "port", a.port, "bridgeid", bridgeID)

	<-ctx.Done()
	return ctx.Err()
}

// interfaceForIP liefert das Multicast-fähige Interface, das die gegebene IP trägt.
func interfaceForIP(ip string) (*net.Interface, error) {
	target := net.ParseIP(ip)
	if target == nil {
		return nil, fmt.Errorf("ungültige IP %q", ip)
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
	return nil, fmt.Errorf("kein multicast-faehiges interface mit IP %s", ip)
}
