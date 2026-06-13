// Package ssdp implements the SSDP/UPnP responder that lets relume present
// itself to the TV as a Gen-2 Hue bridge. It listens on the multicast
// 239.255.255.250:1900, answers M-SEARCH queries immediately and sends
// periodic NOTIFY ssdp:alive broadcasts.
package ssdp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/trick77/relume/internal/config"
)

const (
	multicastAddr = "239.255.255.250:1900"
	// server is the exact SERVER header of a real Hue bridge (verified via diyHue).
	server        = "Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0"
	hassServer    = "Hue/1.0 UPnP/1.0 IpBridge/1.48.0"
	mediaServerST = "urn:schemas-upnp-org:device:MediaServer:1"
	notifyEvery   = 60 * time.Second
)

// Responder answers SSDP requests for a bridge identity.
type Responder struct {
	id       config.Identity
	advIP    string // advertised IP in the LOCATION header
	httpPort int
	log      *slog.Logger
	// Debug enables logging of all received SSDP datagrams including headers
	// (to analyze whether/how a TV searches via SSDP).
	Debug bool
	// BurstDuration enables a diagnostic burst of SSDP NOTIFY messages after
	// startup. Defaults to disabled.
	BurstDuration time.Duration
	// BurstInterval is the interval used during the diagnostic burst.
	BurstInterval time.Duration
	// IdentityProfile selects experimental wire-identity compatibility tweaks.
	// Empty keeps the default; "hass" matches Home Assistant emulated-hue.
	IdentityProfile string
	// MediaServerAlias also advertises/responds as UPnP MediaServer:1. Some
	// Philips Android TVs only emit MediaServer searches during Hue+Ambilight scan.
	MediaServerAlias bool
}

// New creates a Responder. advIP is the IP advertised in the LOCATION header
// (the address of relume's HTTP server).
func New(id config.Identity, advIP string, httpPort int, log *slog.Logger) *Responder {
	return &Responder{id: id, advIP: advIP, httpPort: httpPort, log: log}
}

// Run starts the listener and periodic NOTIFYs and blocks until ctx is cancelled.
func (r *Responder) Run(ctx context.Context) error {
	group := &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}

	// On multi-NIC hosts, listen specifically on the interface that carries the
	// advertised IP — otherwise Go only listens on the system default interface,
	// which is not necessarily on the TV's network.
	iface, err := interfaceForIP(r.advIP)
	if err != nil {
		r.log.Warn("ssdp: interface for advertise IP not found, using default", "advIP", r.advIP, "err", err)
	} else {
		r.log.Info("ssdp: multicast interface selected", "iface", iface.Name, "advIP", r.advIP)
	}

	conn, err := net.ListenMulticastUDP("udp4", iface, group)
	if err != nil {
		return fmt.Errorf("ssdp multicast listen: %w", err)
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(1 << 20)

	go r.notifyLoop(ctx, conn, group)
	if r.BurstDuration > 0 {
		go r.notifyBurst(ctx, conn, group)
	}

	r.log.Info("ssdp responder started", "advertise", r.advIP, "httpPort", r.httpPort)

	buf := make([]byte, 2048)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			r.log.Warn("ssdp read", "err", err)
			continue
		}
		r.handle(conn, src, buf[:n])
	}
}

// logDatagram logs a received SSDP datagram with the headers relevant for
// device detection. This makes it possible to tell whether a TV is searching
// via SSDP (M-SEARCH) or only announcing itself (NOTIFY), and which device it
// is (SERVER/USER-AGENT).
func (r *Responder) logDatagram(src *net.UDPAddr, msg string) {
	firstLine := msg
	if i := strings.IndexByte(firstLine, '\r'); i >= 0 {
		firstLine = firstLine[:i]
	}
	h := parseHeaders(msg)
	r.log.Info("ssdp rx",
		"from", src.String(),
		"line", firstLine,
		"st", h["ST"],
		"man", h["MAN"],
		"nt", h["NT"],
		"nts", h["NTS"],
		"server", h["SERVER"],
		"user-agent", h["USER-AGENT"],
		"location", h["LOCATION"],
	)
}

// parseHeaders splits an SSDP/HTTP message header into headers (keys uppercase).
func parseHeaders(msg string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(msg, "\r\n") {
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(line[:i]))
		out[key] = strings.TrimSpace(line[i+1:])
	}
	return out
}

func (r *Responder) serverHeader() string {
	if r.IdentityProfile == "hass" {
		return hassServer
	}
	return server
}

// interfaceForIP returns the network interface that carries the given IP.
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

// handle answers M-SEARCH queries; everything else is ignored.
func (r *Responder) handle(conn *net.UDPConn, src *net.UDPAddr, data []byte) {
	msg := string(data)
	if r.Debug {
		r.logDatagram(src, msg)
	}
	if !strings.HasPrefix(msg, "M-SEARCH") {
		return
	}
	h := parseHeaders(msg)
	if r.Debug {
		r.log.Info("ssdp: M-SEARCH answered", "to", src.String(), "st", h["ST"], "mediaServerAlias", r.MediaServerAlias)
	}
	// We answer broadly (without strict ST matching), as real bridges do — the
	// TV filters by LOCATION/description.xml. An immediate reply is important
	// because the TV has a short search window (diyHue #988).
	for _, resp := range r.searchResponses() {
		if _, err := conn.WriteToUDP([]byte(resp), src); err != nil {
			r.log.Warn("ssdp respond", "err", err, "to", src.String())
			return
		}
		if r.Debug {
			h := parseHeaders(resp)
			r.log.Info("ssdp tx",
				"to", src.String(),
				"kind", "search-response",
				"st", h["ST"],
				"usn", h["USN"],
				"location", h["LOCATION"],
				"cache-control", h["CACHE-CONTROL"],
			)
		}
	}
}

// searchResponses returns the M-SEARCH 200 OK responses for the configured SSDP variants.
func (r *Responder) searchResponses() []string {
	variants := r.ssdpVariants()
	out := make([]string, 0, len(variants))
	for _, v := range variants {
		out = append(out, "HTTP/1.1 200 OK\r\n"+
			"HOST: 239.255.255.250:1900\r\n"+
			"EXT:\r\n"+
			fmt.Sprintf("CACHE-CONTROL: max-age=%d\r\n", v.maxAge())+
			fmt.Sprintf("LOCATION: %s\r\n", r.location(v))+
			"SERVER: "+r.serverHeader()+"\r\n"+
			"hue-bridgeid: "+r.id.BridgeID()+"\r\n"+
			"ST: "+v.st+"\r\n"+
			"USN: "+v.usn+"\r\n"+
			"\r\n")
	}
	return out
}

type ssdpVariant struct {
	st  string
	nt  string
	usn string
}

func (v ssdpVariant) maxAge() int {
	if v.st == mediaServerST || v.nt == mediaServerST {
		return 1
	}
	return 100
}

func (r *Responder) location(v ssdpVariant) string {
	location := fmt.Sprintf("http://%s:%d/description.xml", r.advIP, r.httpPort)
	if v.st == mediaServerST || v.nt == mediaServerST {
		return location + "?relume=ms1"
	}
	return location
}

func (r *Responder) ssdpVariants() []ssdpVariant {
	uuid := r.id.UUID()
	variants := []ssdpVariant{
		{st: "upnp:rootdevice", nt: "upnp:rootdevice", usn: "uuid:" + uuid + "::upnp:rootdevice"},
		{st: "uuid:" + uuid, nt: "uuid:" + uuid, usn: "uuid:" + uuid},
		{st: "urn:schemas-upnp-org:device:basic:1", nt: "urn:schemas-upnp-org:device:basic:1", usn: "uuid:" + uuid},
	}
	if r.MediaServerAlias {
		variants = append(variants, ssdpVariant{
			st:  mediaServerST,
			nt:  mediaServerST,
			usn: "uuid:" + uuid + "::" + mediaServerST,
		})
	}
	return variants
}

// notifyLoop periodically sends NOTIFY ssdp:alive to the multicast.
func (r *Responder) notifyLoop(ctx context.Context, conn *net.UDPConn, group *net.UDPAddr) {
	t := time.NewTicker(notifyEvery)
	defer t.Stop()
	r.sendNotify(conn, group)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sendNotify(conn, group)
		}
	}
}

func (r *Responder) notifyBurst(ctx context.Context, conn *net.UDPConn, group *net.UDPAddr) {
	interval := r.BurstInterval
	if interval <= 0 {
		interval = time.Second
	}
	r.log.Info("ssdp: discovery burst started", "duration", r.BurstDuration, "interval", interval)
	runBurst(ctx, interval, r.BurstDuration, func() {
		r.sendNotify(conn, group)
		if r.Debug {
			r.log.Info("ssdp: discovery burst notify sent")
		}
	})
	r.log.Info("ssdp: discovery burst finished")
}

func runBurst(ctx context.Context, interval, duration time.Duration, send func()) {
	if duration <= 0 {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	t := time.NewTicker(interval)
	defer t.Stop()

	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-t.C:
			send()
		}
	}
}

func (r *Responder) sendNotify(conn *net.UDPConn, group *net.UDPAddr) {
	for _, msg := range r.notifyMessages() {
		if _, err := conn.WriteToUDP([]byte(msg), group); err != nil {
			r.log.Warn("ssdp notify", "err", err)
			return
		}
		if r.Debug {
			h := parseHeaders(msg)
			r.log.Info("ssdp tx",
				"to", group.String(),
				"kind", "notify",
				"nt", h["NT"],
				"usn", h["USN"],
				"location", h["LOCATION"],
				"cache-control", h["CACHE-CONTROL"],
			)
		}
	}
}

func (r *Responder) notifyMessages() []string {
	variants := r.ssdpVariants()
	msgs := make([]string, 0, len(variants))
	for _, v := range variants {
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			fmt.Sprintf("CACHE-CONTROL: max-age=%d\r\n", v.maxAge()) +
			fmt.Sprintf("LOCATION: %s\r\n", r.location(v)) +
			"SERVER: " + r.serverHeader() + "\r\n" +
			"NTS: ssdp:alive\r\n" +
			"hue-bridgeid: " + r.id.BridgeID() + "\r\n" +
			"NT: " + v.nt + "\r\n" +
			"USN: " + v.usn + "\r\n" +
			"\r\n"
		msgs = append(msgs, msg)
	}
	return msgs
}
