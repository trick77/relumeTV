// Package diag contains diagnostic helpers to analyze the discovery behavior of
// clients (in particular Ambilight TVs): which method does a TV use to find a
// Hue bridge — local SSDP, mDNS or the Philips cloud?
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

// MDNSObserver passively listens for mDNS and logs Hue-related queries
// (_hue._tcp). It does NOT respond — it only serves to analyze whether a TV
// searches for the bridge via mDNS.
type MDNSObserver struct {
	advIP     string
	log       *slog.Logger
	DebugTVIP string
}

// NewMDNSObserver creates an Observer; advIP selects the interface (multi-NIC).
func NewMDNSObserver(advIP string, log *slog.Logger) *MDNSObserver {
	return &MDNSObserver{advIP: advIP, log: log}
}

// Run listens until ctx is cancelled.
func (o *MDNSObserver) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", mdnsGroup)
	if err != nil {
		return err
	}
	iface, ierr := interfaceForIP(o.advIP)
	if ierr != nil {
		o.log.Warn("mdns: interface for advertise IP not found, using default", "err", ierr)
	}
	conn, err := net.ListenMulticastUDP("udp4", iface, addr)
	if err != nil {
		return fmt.Errorf("mdns listen: %w", err)
	}
	defer conn.Close()
	o.log.Info("mdns observer started (listening for _hue._tcp queries)")

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

// inspect parses the questions of an mDNS message and logs Hue references.
func (o *MDNSObserver) inspect(src *net.UDPAddr, msg []byte) {
	names := dnsQuestionNames(msg)
	if !o.shouldLogMDNS(src.IP, names) {
		return
	}
	for _, name := range names {
		o.log.Info("mdns: query", "from", src.IP.String(), "name", name)
	}
}

func (o *MDNSObserver) shouldLogMDNS(src net.IP, names []string) bool {
	if o.DebugTVIP != "" && src.Equal(net.ParseIP(o.DebugTVIP)) {
		return len(names) > 0
	}
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), "hue") {
			return true
		}
	}
	return false
}

// dnsQuestionNames extracts the QNAMEs from a DNS/mDNS message (best effort).
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

// readName reads a (possibly compressed) DNS name starting at offset off.
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
		case l&0xC0 == 0xC0: // Pointer (compression)
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
