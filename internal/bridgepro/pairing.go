package bridgepro

import (
	"context"
	"fmt"
	"time"

	"github.com/trick77/relume-tv/internal/config"
)

// Pairer completes the Hue Bridge Pro pairing handshake on an already-pinned config
// shell (Host + CertSHA256 set, no keys yet). It polls Pair until the user taps the
// Pro's physical link button — the one step that cannot be automated — then fills in
// the app key, client key and, best-effort, the Pro's name and bridge id.
//
// It centralises the wait-for-button → set-keys → capture sequence that the serve
// auto-pairing and the setup command previously duplicated verbatim (the most
// correctness-sensitive code: it persists the credentials that authenticate every
// later request). Discovery and certificate pinning stay with the caller, whose retry
// policies for those steps differ. The seam fields (pair, capture) carry production
// defaults and are overridden in tests so the loop is exercisable without a live Pro.
type Pairer struct {
	// DeviceType is the application identifier sent to the Pro (e.g. "relume-tv#host").
	DeviceType string
	// Interval is the delay between link-button poll attempts.
	Interval time.Duration

	pair    func(pro *config.BridgePro, deviceType string) (*PairResult, error)
	capture func(pro *config.BridgePro)
}

// NewPairer builds a Pairer with the production seams: it pairs over the pinned
// HTTPS client and captures the Pro's name/id once paired.
func NewPairer(deviceType string) *Pairer {
	return &Pairer{
		DeviceType: deviceType,
		Interval:   3 * time.Second,
		pair: func(pro *config.BridgePro, deviceType string) (*PairResult, error) {
			return Pair(HTTPClientFor(pro), pro.Host, deviceType)
		},
		capture: func(pro *config.BridgePro) {
			if name, id, err := New(pro).BridgeInfo(); err == nil {
				pro.Name, pro.BridgeID = name, id
			}
		},
	}
}

// WaitForLinkButton polls until pairing succeeds, ctx is cancelled, or — when
// deadline is non-zero — the deadline passes. On success it mutates pro in place
// (AppKey, ClientKey, Name, BridgeID) and returns it. onAttempt, if set, is called
// after each failed attempt with the zero-based attempt index, for progress output
// (a periodic log line in serve, a "." in setup).
func (p *Pairer) WaitForLinkButton(ctx context.Context, pro *config.BridgePro, deadline time.Time, onAttempt func(attempt int)) (*config.BridgePro, error) {
	for attempt := 0; ; attempt++ {
		res, err := p.pair(pro, p.DeviceType)
		if err == nil {
			pro.AppKey = res.AppKey
			pro.ClientKey = res.ClientKey
			p.capture(pro)
			return pro, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, fmt.Errorf("pairing failed (press the Hue Bridge Pro link button in time): %w", err)
		}
		if onAttempt != nil {
			onAttempt(attempt)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.Interval):
		}
	}
}
