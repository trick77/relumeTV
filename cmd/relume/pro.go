package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/trick77/relume/internal/bridge"
	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/clipv1"
	"github.com/trick77/relume/internal/config"
)

// resolveProHost determines the Bridge Pro's current host. When bridgeIP is set
// it is returned verbatim (no discovery, empty discoveryID). Otherwise it runs
// cloud discovery and applies the M6 id-matching:
//
//   - want != "" (a reconnect with a stored DiscoveryID): pick the discovered
//     bridge whose id == want. If none matches, return an empty host (the caller
//     retries) — never fall back to bridges[0], which could target a DIFFERENT
//     bridge on a multi-bridge LAN, the exact thing DiscoveryID exists to prevent.
//   - want == "" (initial pairing, or a legacy install with no stored id): pick
//     bridges[0] and LOG that fallback so multi-bridge LANs are diagnosable.
//
// On success it returns the chosen host plus the discovered bridge's id so the
// caller can persist it (DiscoveryID) for future reconnects.
func resolveProHost(bridgeIP, want string, discover func() ([]bridgepro.DiscoveredBridge, error), log *slog.Logger) (host, discoveryID string, err error) {
	if bridgeIP != "" {
		return bridgeIP, "", nil
	}
	bridges, derr := discover()
	if derr != nil {
		return "", "", derr
	}
	if len(bridges) == 0 {
		return "", "", nil
	}
	if want != "" {
		for _, b := range bridges {
			if b.ID == want {
				return b.InternalIPAddress, b.ID, nil
			}
		}
		// Stored id not among the discovered bridges: do not guess another bridge.
		return "", "", nil
	}
	// Legacy fallback: no stored DiscoveryID, so pick the first discovered bridge.
	// Always log it (louder when several are present, since that is the case where
	// blindly taking the first risks the wrong bridge on a multi-bridge LAN).
	if len(bridges) > 1 {
		log.Warn("hue bridge pro discovery via Philips cloud (discovery.meethue.com): no stored discovery id, multiple bridges found — picking the first; pass -bridge-ip to disambiguate",
			"count", len(bridges), "picked", bridges[0].InternalIPAddress)
	} else {
		log.Info("hue bridge pro discovery via Philips cloud (discovery.meethue.com): no stored discovery id — picking the only discovered bridge",
			"picked", bridges[0].InternalIPAddress)
	}
	return bridges[0].InternalIPAddress, bridges[0].ID, nil
}

// pinProShell builds the *config.BridgePro shell for a host: it pins the leaf
// certificate (unless skipTLS) and returns the shell ready for Pair. discoveryID
// is the cloud-discovery id (empty when -bridge-ip was used) and is carried onto
// the shell so it survives the eventual SetPro.
func pinProShell(host, discoveryID string, skipTLS bool, fetchFingerprint func(host string) (string, error)) (*config.BridgePro, error) {
	pro := &config.BridgePro{Host: host, SkipTLSVerify: skipTLS, DiscoveryID: discoveryID}
	if !skipTLS {
		fp, ferr := fetchFingerprint(host)
		if ferr != nil {
			return nil, ferr
		}
		pro.CertSHA256 = fp
	}
	return pro, nil
}

// proWatcher keeps the already-paired Bridge Pro reachable. It health-checks
// periodically and, on a genuine unreachable failure, re-discovers the Pro's
// current IP, re-pins its certificate and hot-swaps the light provider — all
// without a new button press, since the stored appKey/clientKey stay valid
// across reboots and DHCP IP changes.
//
// The seam fields (healthCheck, discover, fetchFingerprint, applyProvider) carry
// real defaults in production and are overridden in tests so the resilience cycle
// is exercisable without a live Pro or network.
type proWatcher struct {
	cfg        *config.Config
	clip       *clipv1.Server
	controlled *bridge.ControlledSet
	liveColors *liveColors
	stats      *proStats
	bridgeIP   string
	skipTLS    bool
	log        *slog.Logger

	healthCheck      func(*config.BridgePro) error
	discover         func() ([]bridgepro.DiscoveredBridge, error)
	fetchFingerprint func(host string) (string, error)
	applyProvider    func(*config.BridgePro)
}

// newProWatcher constructs a proWatcher with the production seams wired in.
func newProWatcher(cfg *config.Config, clip *clipv1.Server, controlled *bridge.ControlledSet, live *liveColors, stats *proStats, bridgeIP string, skipTLS bool, log *slog.Logger) *proWatcher {
	w := &proWatcher{
		cfg:        cfg,
		clip:       clip,
		controlled: controlled,
		liveColors: live,
		stats:      stats,
		bridgeIP:   bridgeIP,
		skipTLS:    skipTLS,
		log:        log,
	}
	w.healthCheck = func(p *config.BridgePro) error {
		// Liveness probe only: BridgeInfo reads the single bridge resource, which is
		// far lighter than Lights() (the full light list) and enough to prove the Pro
		// is reachable. Failure modes (unreachable / queue-full / domain) are what
		// tick() switches on; the returned name/id are irrelevant here.
		_, _, err := bridgepro.New(p).BridgeInfo()
		return err
	}
	w.discover = bridgepro.Discover
	w.fetchFingerprint = bridgepro.FetchLeafFingerprint
	w.applyProvider = func(p *config.BridgePro) {
		clip.SetLightProvider(newProvider(bridgepro.New(p), controlled, live, stats, log))
	}
	return w
}

// run drives the health-check/reconnect cycle on a 60s interval until ctx is
// cancelled. It backfills the Pro's name/id once up front (for installs paired
// before those were captured), then loops calling tick.
func (w *proWatcher) run(ctx context.Context) {
	const checkInterval = 60 * time.Second
	pro := w.cfg.GetPro()
	if pro == nil {
		return
	}
	// Backfill the Pro's name/id for installs paired before they were captured, so
	// logs can reference it. Best-effort and only while the Pro is reachable. Build a
	// fresh *BridgePro and SetPro it rather than mutating the snapshot in place —
	// GetPro promises an immutable view to concurrent readers (monitorIdle, shutdown).
	if pro.Name == "" && pro.BridgeID == "" {
		if name, id, ierr := bridgepro.New(pro).BridgeInfo(); ierr == nil && (name != "" || id != "") {
			updated := *pro
			updated.Name, updated.BridgeID = name, id
			if serr := w.cfg.SetPro(&updated); serr != nil {
				w.log.Warn("persisting hue bridge pro name/id", "err", serr)
			}
		}
	}
	for sleepCtx(ctx, checkInterval) {
		w.tick()
	}
}

// tick runs ONE health-check + maybe-reconnect cycle and reports whether it
// reconnected the Pro to a new pairing. It reads the current pairing via
// cfg.GetPro so a reconnect from a previous tick flows forward automatically.
func (w *proWatcher) tick() (reconnected bool) {
	pro := w.cfg.GetPro()
	if pro == nil {
		return false
	}

	err := w.healthCheck(pro)
	if err == nil {
		return false // still reachable
	}
	// M3: a 503 "command queue full" (or a per-attribute domain rejection) means the
	// Pro is reachable but busy. Re-discovering / re-pinning then is harmful churn —
	// log and back off WITHOUT touching the pairing. Only a genuinely unreachable Pro
	// (or any non-queue/non-domain error) proceeds to re-discover.
	if errors.Is(err, bridgepro.ErrQueueFull) || errors.Is(err, bridgepro.ErrDomain) {
		w.log.Warn("hue bridge pro busy (command queue full) — not re-discovering; backing off", "", pro, "err", err)
		return false
	}

	w.log.Warn("hue bridge pro not reachable — is it turned off? "+
		"Turn it back on (or check its power/network cable); "+
		"relume can't control the lights until it is back. Retrying.", "", pro, "err", err)

	host, discoveryID, derr := resolveProHost(w.bridgeIP, pro.DiscoveryID, w.discover, w.log)
	if derr != nil || host == "" {
		if pro.DiscoveryID != "" && w.bridgeIP == "" {
			w.log.Warn("hue bridge pro reconnect: stored bridge not found via discovery; will retry", "discoveryId", pro.DiscoveryID, "err", derr)
		} else {
			w.log.Warn("hue bridge pro reconnect: not found via discovery; will retry", "err", derr)
		}
		return false
	}

	certSHA := pro.CertSHA256
	if !w.skipTLS && !pro.SkipTLSVerify {
		fp, ferr := w.fetchFingerprint(host)
		if ferr != nil {
			w.log.Warn("hue bridge pro reconnect: cert fetch failed; will retry", "host", host, "err", ferr)
			return false
		}
		certSHA = fp
	}

	updated := reconnectProConfig(pro, host, certSHA, w.skipTLS)
	// A discovered id (when discovery, not -bridge-ip, was used) refreshes the stored
	// one; otherwise reconnectProConfig already carried the old DiscoveryID forward.
	if discoveryID != "" {
		updated.DiscoveryID = discoveryID
	}
	if verr := w.healthCheck(updated); verr != nil {
		w.log.Warn("hue bridge pro reconnect: still unreachable", "host", host, "err", verr)
		return false
	}
	if serr := w.cfg.SetPro(updated); serr != nil {
		w.log.Error("persisting reconnected hue bridge pro", "err", serr)
		return false
	}
	w.applyProvider(updated)
	w.log.Info("hue bridge pro reconnected", "", updated)
	return true
}
