// Package entertainment implements the TV-facing side of the Hue Entertainment
// path: a DTLS-PSK server on UDP :2100 that decrypts the Ambilight stream and
// decodes its HueStream frames. This first phase only logs the decoded frames;
// forwarding the colors to the Bridge Pro follows in a later phase.
package entertainment

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/trick77/relume/internal/huestream"
)

// KeyLookup returns the DTLS pre-shared key for a client identity (the Hue
// username / application key the TV presents) and whether it is known.
type KeyLookup func(identity string) ([]byte, bool)

// Receiver is the DTLS-PSK server on udp :2100. The PSK is the clientkey relume
// minted for the TV at pairing, so relume can decrypt the stream.
type Receiver struct {
	bindIP string
	lookup KeyLookup
	log    *slog.Logger
	// Port overrides the listen port (default 2100). For tests.
	Port int
	// OnFrame, if set, is called for every decoded frame (in addition to logging).
	// A later phase wires this to forward the colors to the Bridge Pro.
	OnFrame func(remote string, f *huestream.Frame)
	// OnActivity, if set, is called once per decoded frame — used to feed the
	// idle-off monitor so a streaming TV is not treated as idle.
	OnActivity func()
	// OnStreamStart / OnStreamStop, if set, bracket a TV DTLS connection — Phase C
	// wires these to establish / tear down relume's own stream to the Pro so the
	// Pro area lives exactly as long as the TV is streaming.
	OnStreamStart func(remote string)
	OnStreamStop  func(remote string)
}

// NewReceiver creates the receiver. bindIP is the advertised IP (pins the socket
// to the TV-facing interface on a multi-homed host).
func NewReceiver(bindIP string, lookup KeyLookup, log *slog.Logger) *Receiver {
	return &Receiver{bindIP: bindIP, lookup: lookup, log: log}
}

// Run listens for the entertainment DTLS stream until ctx is cancelled.
func (r *Receiver) Run(ctx context.Context) error {
	port := r.Port
	if port == 0 {
		port = 2100
	}
	addr := &net.UDPAddr{IP: net.ParseIP(r.bindIP), Port: port}

	psk := func(hint []byte) ([]byte, error) {
		id := string(hint)
		key, ok := r.lookup(id)
		if !ok {
			r.log.Warn("entertainment: unknown DTLS identity", "identity", id)
			return nil, fmt.Errorf("unknown psk identity %q", id)
		}
		r.log.Info("entertainment: DTLS client authenticating", "identity", id)
		return key, nil
	}

	// Hue uses DTLS 1.2 PSK with TLS_PSK_WITH_AES_128_GCM_SHA256, and its clients
	// do not use the extended master secret — disable it for compatibility.
	listener, err := dtls.ListenWithOptions("udp", addr,
		dtls.WithPSK(psk),
		dtls.WithCipherSuites(dtls.TLS_PSK_WITH_AES_128_GCM_SHA256),
		dtls.WithExtendedMasterSecret(dtls.DisableExtendedMasterSecret),
	)
	if err != nil {
		return fmt.Errorf("entertainment dtls listen on %s: %w", addr, err)
	}
	defer listener.Close()
	r.log.Info("entertainment receiver started (DTLS-PSK on udp :2100)", "bind", addr.String())

	go func() { <-ctx.Done(); _ = listener.Close() }()

	for {
		conn, aerr := listener.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.log.Warn("entertainment accept", "err", aerr)
			continue
		}
		go r.handle(ctx, conn)
	}
}

func (r *Receiver) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()

	if dc, ok := conn.(*dtls.Conn); ok {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := dc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			r.log.Warn("entertainment handshake failed", "from", remote, "err", err)
			return
		}
	}
	r.log.Info("entertainment stream connected", "from", remote)
	if r.OnStreamStart != nil {
		r.OnStreamStart(remote)
	}
	if r.OnStreamStop != nil {
		defer r.OnStreamStop(remote)
	}

	var (
		mu          sync.Mutex
		frames      uint64
		last        *huestream.Frame
		firstLogged bool
	)
	done := make(chan struct{})

	// Reader goroutine: decrypt + parse frames (~25/s) into shared state.
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			n, rerr := conn.Read(buf)
			if rerr != nil {
				return
			}
			f, perr := huestream.Parse(buf[:n])
			if perr != nil {
				r.log.Warn("entertainment: bad frame", "from", remote, "bytes", n, "err", perr)
				continue
			}
			if r.OnActivity != nil {
				r.OnActivity()
			}
			if r.OnFrame != nil {
				r.OnFrame(remote, f)
			}
			mu.Lock()
			frames++
			last = f
			logFirst := !firstLogged
			firstLogged = true
			mu.Unlock()
			if logFirst {
				r.log.Info("entertainment: first frame decoded", "from", remote,
					"version", fmt.Sprintf("%d.%d", f.Major, f.Minor),
					"colorspace", f.ColorSpaceName(), "channels", len(f.Channels),
					"config_id", f.ConfigID, "sample", sample(f))
			}
		}
	}()

	// Summarize the high-rate stream every 5s instead of logging each frame.
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var prev uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			mu.Lock()
			total := frames
			mu.Unlock()
			r.log.Info("entertainment stream closed", "from", remote, "frames_total", total)
			return
		case <-t.C:
			mu.Lock()
			total, f := frames, last
			mu.Unlock()
			if f != nil && total != prev {
				r.log.Info("entertainment stream", "from", remote,
					"frames_5s", total-prev, "channels", len(f.Channels),
					"colorspace", f.ColorSpaceName(), "sample", sample(f))
				prev = total
			}
		}
	}
}

// sample formats the first channel's color, scaled to 8-bit, for readable logs.
func sample(f *huestream.Frame) string {
	if len(f.Channels) == 0 {
		return ""
	}
	c := f.Channels[0]
	return fmt.Sprintf("ch%d=%d/%d/%d", c.ID, c.A>>8, c.B>>8, c.C>>8)
}
