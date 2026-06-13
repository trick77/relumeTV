// Package diag enthält Diagnose-Helfer, um das Discovery-Verhalten von Clients
// (insbesondere Ambilight-TVs) zu analysieren: Welches Verfahren nutzt ein TV, um
// eine Hue-Bridge zu finden — lokales SSDP, mDNS oder die Philips-Cloud?
package diag

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

const mdnsGroup = "224.0.0.251:5353"

// MDNSObserver lauscht passiv auf mDNS und protokolliert Hue-bezogene Anfragen
// (_hue._tcp). Er antwortet NICHT — er dient nur der Analyse, ob ein TV per mDNS
// nach der Bridge sucht.
type MDNSObserver struct {
	advIP string
	log   *slog.Logger
}

// NewMDNSObserver erstellt einen Observer; advIP wählt das Interface (Multi-NIC).
func NewMDNSObserver(advIP string, log *slog.Logger) *MDNSObserver {
	return &MDNSObserver{advIP: advIP, log: log}
}

// Run lauscht bis ctx beendet wird.
func (o *MDNSObserver) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", mdnsGroup)
	if err != nil {
		return err
	}
	iface, ierr := interfaceForIP(o.advIP)
	if ierr != nil {
		o.log.Warn("mdns: interface zur advertise-ip nicht gefunden, nutze default", "err", ierr)
	}
	conn, err := net.ListenMulticastUDP("udp4", iface, addr)
	if err != nil {
		return fmt.Errorf("mdns listen: %w", err)
	}
	defer conn.Close()
	o.log.Info("mdns-observer gestartet (lauscht auf _hue._tcp-Anfragen)")

	buf := make([]byte, 65536)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_ = conn.SetReadDeadline(deadline())
		n, src, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				continue
			}
			continue
		}
		o.inspect(src, buf[:n])
	}
}

func deadline() time.Time { return time.Now().Add(time.Second) }

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

// inspect parst die Fragen einer mDNS-Nachricht und loggt Hue-Bezüge.
func (o *MDNSObserver) inspect(src *net.UDPAddr, msg []byte) {
	names := dnsQuestionNames(msg)
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), "hue") {
			o.log.Info("mdns: hue-bezogene anfrage", "from", src.IP.String(), "name", name)
		}
	}
}

// dnsQuestionNames extrahiert die QNAMEs aus einer DNS/mDNS-Nachricht (best effort).
func dnsQuestionNames(msg []byte) []string {
	if len(msg) < 12 {
		return nil
	}
	qd := int(msg[4])<<8 | int(msg[5])
	off := 12
	var names []string
	for q := 0; q < qd && off < len(msg); q++ {
		name, next, ok := readName(msg, off)
		if !ok {
			break
		}
		names = append(names, name)
		off = next + 4 // QTYPE(2) + QCLASS(2)
	}
	return names
}

// readName liest einen (ggf. komprimierten) DNS-Namen ab Offset off.
func readName(msg []byte, off int) (string, int, bool) {
	var labels []string
	jumped := false
	next := off
	for off < len(msg) {
		l := int(msg[off])
		switch {
		case l == 0:
			off++
			if !jumped {
				next = off
			}
			return strings.Join(labels, "."), next, true
		case l&0xC0 == 0xC0: // Pointer (Komprimierung)
			if off+1 >= len(msg) {
				return "", off, false
			}
			if !jumped {
				next = off + 2
			}
			off = int(l&0x3F)<<8 | int(msg[off+1])
			jumped = true
		default:
			if off+1+l > len(msg) {
				return "", off, false
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", off, false
}
