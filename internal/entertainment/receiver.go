// Package entertainment implements the TV-facing side of the Hue Entertainment
// path: a DTLS-PSK server on UDP :2100 that decrypts the Ambilight stream and
// decodes its HueStream frames. This first phase only logs the decoded frames;
// forwarding the colors to the Hue Bridge Pro follows in a later phase.
package entertainment

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/trick77/relumetv/internal/huestream"
)

// KeyLookup returns the DTLS pre-shared key for a client identity (the Hue
// username / application key the TV presents) and whether it is known.
type KeyLookup func(identity string) ([]byte, bool)

// Receiver is the DTLS-PSK server on udp :2100. The PSK is the clientkey relumeTV
// minted for the TV at pairing, so relumeTV can decrypt the stream.
type Receiver struct {
	bindIP string
	lookup KeyLookup
	log    *slog.Logger
	// Port overrides the listen port (default 2100). For tests.
	Port int
	// OnFrame, if set, is called for every decoded frame (in addition to logging).
	// A later phase wires this to forward the colors to the Hue Bridge Pro.
	OnFrame func(remote string, f *huestream.Frame)
	// OnActivity, if set, is called once per decoded frame — used to feed the
	// idle-off monitor so a streaming TV is not treated as idle.
	OnActivity func()
	// OnStreamStart / OnStreamStop, if set, bracket a TV DTLS connection — Phase C
	// wires these to establish / tear down relumeTV's own stream to the Pro so the
	// Pro area lives exactly as long as the TV is streaming.
	OnStreamStart func(remote string)
	OnStreamStop  func(remote string)
	// OnWindowStats, if set, is called once per 5s rollup with the largest brightness
	// and colour jump on the *incoming* TV stream over that window. The streamer reports
	// the same on its sent (smoothed) stream; the gap is the jitter the easing removed.
	OnWindowStats func(briJump, colJump uint32)
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
		dropped     uint64
		last        *huestream.Frame
		firstLogged bool
		// Per-window frame stats (reset each 5s rollup) to tell genuine flicker —
		// brightness dipping toward 0 (black flashes) — from merely hard-but-correct
		// colour jumps. briMin starts at the max sentinel so the first frame lowers it.
		briMin   uint32 = 0xFFFF
		briMax   uint32
		briJump  uint32 // largest |Δbrightness| between consecutive frames
		colJump  uint32 // largest colour jump between consecutive frames
		nearZero uint64 // channel samples below nearZeroBri (a black-flash indicator)
	)
	done := make(chan struct{})

	// Decouple decode from forward. OnFrame (the sink that forwards to the Pro) must
	// never block the reader: a stalled sink would back up the single reader goroutine
	// and the kernel would silently drop UDP with no visibility. Frames flow through a
	// small bounded queue handled by a separate forwarder goroutine; when the queue is
	// full the newest frame is dropped (Ambilight frames are ephemeral — latest wins)
	// and counted, so a slow sink degrades smoothly and observably rather than stalling.
	const frameQueueSize = 8
	var frameCh chan *huestream.Frame
	fwdDone := make(chan struct{})
	if r.OnFrame != nil {
		frameCh = make(chan *huestream.Frame, frameQueueSize)
		go func() {
			defer close(fwdDone)
			for f := range frameCh {
				r.OnFrame(remote, f)
			}
		}()
	} else {
		close(fwdDone)
	}

	// Reader goroutine: decrypt + parse frames (~25/s) into shared state.
	go func() {
		defer close(done)
		if frameCh != nil {
			defer close(frameCh) // let the forwarder drain and exit
		}
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
			if frameCh != nil {
				select {
				case frameCh <- f:
				default:
					mu.Lock()
					dropped++
					mu.Unlock()
				}
			}
			mu.Lock()
			// Accumulate per-window brightness/jump stats. Brightness extremes come
			// from this frame; jumps are measured against the previous frame (last).
			for _, ch := range f.Channels {
				b := brightness(f.ColorSpace, ch)
				if b < briMin {
					briMin = b
				}
				if b > briMax {
					briMax = b
				}
				if b < nearZeroBri {
					nearZero++
				}
			}
			if last != nil && len(last.Channels) == len(f.Channels) {
				for i := range f.Channels {
					cur, prv := f.Channels[i], last.Channels[i]
					if d := absDiff(brightness(f.ColorSpace, cur), brightness(last.ColorSpace, prv)); d > briJump {
						briJump = d
					}
					cj := absDiff(uint32(cur.A), uint32(prv.A)) + absDiff(uint32(cur.B), uint32(prv.B))
					if f.ColorSpace != huestream.ColorSpaceXY {
						cj += absDiff(uint32(cur.C), uint32(prv.C))
					}
					if cj > colJump {
						colJump = cj
					}
				}
			}
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
			<-fwdDone // let the forwarder drain queued frames before OnStreamStop
			mu.Lock()
			total, drops := frames, dropped
			mu.Unlock()
			r.log.Info("entertainment stream closed", "from", remote, "frames_total", total, "frames_dropped", drops)
			return
		case <-t.C:
			mu.Lock()
			total, drops, f := frames, dropped, last
			bMin, bMax, bJump, cJump, nz := briMin, briMax, briJump, colJump, nearZero
			// Reset the window accumulators for the next interval.
			briMin, briMax, briJump, colJump, nearZero = 0xFFFF, 0, 0, 0, 0
			mu.Unlock()
			if r.OnWindowStats != nil {
				r.OnWindowStats(bJump, cJump)
			}
			if f != nil && total != prev {
				r.log.Debug("entertainment stream", "from", remote,
					"frames_5s", total-prev, "frames_dropped", drops, "channels", len(f.Channels),
					"colorspace", f.ColorSpaceName(),
					"bri_min", bMin, "bri_max", bMax, "bri_max_jump", bJump,
					"col_max_jump", cJump, "near_zero", nz, "sample", sample(f))
				prev = total
			}
		}
	}
}

// nearZeroBri is the raw 16-bit brightness below which a channel counts as a
// near-black sample (~3% of full) — used to flag black-flash flicker.
const nearZeroBri = 2048

// brightness returns a channel's brightness for the frame's colour space: xy
// frames carry it directly in C; rgb frames approximate it as max(R,G,B).
func brightness(colorSpace uint8, c huestream.Channel) uint32 {
	if colorSpace == huestream.ColorSpaceXY {
		return uint32(c.C)
	}
	m := c.A
	if c.B > m {
		m = c.B
	}
	if c.C > m {
		m = c.C
	}
	return uint32(m)
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// sample formats the first channel's color, scaled to 8-bit, for readable logs.
func sample(f *huestream.Frame) string {
	if len(f.Channels) == 0 {
		return ""
	}
	c := f.Channels[0]
	return fmt.Sprintf("ch%d=%d/%d/%d", c.ID, c.A>>8, c.B>>8, c.C>>8)
}
