package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/trick77/relume-tv/internal/bridge"
	"github.com/trick77/relume-tv/internal/bridgepro"
	"github.com/trick77/relume-tv/internal/clipv1"
	"github.com/trick77/relume-tv/internal/config"
)

// Setup precondition sentinels for the initial pairing selection (selectProForPairing).
// They let autoPairPro classify a failed discovery into the three wizard-reportable
// preconditions: (a) no bridge, (b) not a Hue Bridge Pro, (c) mDNS discovery unavailable.
var (
	// ErrNoProBridge: discovery returned bridge(s) but none is a Hue Bridge Pro
	// (their modelid is not BSB003). The wrapped message names the observed modelid(s).
	ErrNoProBridge = errors.New("no hue bridge pro (BSB003) among the discovered bridges")
	// ErrProModelUnknown: bridge(s) were discovered but none could be queried for its
	// modelid (unreachable at :443). Distinct from ErrNoProBridge so the precondition
	// reads (c)-ish "found but unreachable", not the misleading (b) "not a Pro".
	ErrProModelUnknown = errors.New("discovered bridge(s) unreachable to read modelid")
)

// resolveProHost determines a paired Hue Bridge Pro's current host for a RECONNECT,
// via local mDNS discovery (the only discovery path now — both the manual -bridge-ip
// override and the Philips cloud were removed). It applies the M6 id-matching: it picks
// the discovered bridge whose id == want. If want is empty or none matches, it returns
// an empty host (the caller retries) — it never falls back to bridges[0], which could
// target a DIFFERENT bridge on a multi-bridge LAN, the exact thing DiscoveryID exists to
// prevent.
//
// On success it returns the chosen host plus the discovered bridge's id so the caller
// can persist it (DiscoveryID) for future reconnects.
func resolveProHost(want string, discover func() ([]bridgepro.DiscoveredBridge, error)) (host, discoveryID string, err error) {
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
	}
	// No stored id, or the stored id is not among the discovered bridges: do not guess
	// another bridge.
	return "", "", nil
}

// selectProForPairing runs mDNS discovery for the INITIAL pairing and returns the
// first discovered bridge that is really a Hue Bridge Pro (modelid BSB003), folding the
// Pro check into the multi-bridge selection (so it never blindly pairs bridges[0], which
// could be a non-Pro Hue bridge on the LAN). It returns the chosen host, its discovery
// id, and — for the wizard banner and diagnostics — the observed modelid.
//
// It uses the modelid the bridge advertised in its mDNS TXT record when present, and
// only falls back to an HTTP /api/0/config read (fetchModel) when the TXT lacked it.
//
// Error classification for the wizard preconditions:
//   - discover error            → returned verbatim          → (c) discovery unavailable
//   - len(bridges) == 0         → host=="" , err==nil        → (a) no bridge found
//   - bridge(s) responded, none BSB003 → ErrNoProBridge      → (b) not a Hue Bridge Pro
//   - bridge(s) found, none reachable for modelid → ErrProModelUnknown → (c)-ish
func selectProForPairing(discover func() ([]bridgepro.DiscoveredBridge, error), fetchModel func(host string) (string, error), log *slog.Logger) (host, discoveryID, modelID string, err error) {
	bridges, derr := discover()
	if derr != nil {
		return "", "", "", derr
	}
	if len(bridges) == 0 {
		return "", "", "", nil
	}
	anyResponded := false
	seen := make([]string, 0, len(bridges))
	for _, b := range bridges {
		mid := b.ModelID
		if mid == "" {
			// The bridge didn't advertise modelid over mDNS — confirm over HTTP.
			m, merr := fetchModel(b.InternalIPAddress)
			if merr != nil {
				log.Warn("hue bridge pro selection: could not read modelid (bridge unreachable?)", "host", b.InternalIPAddress, "err", merr)
				continue
			}
			mid = m
		}
		anyResponded = true
		if mid == bridgepro.ModelHueBridgePro {
			return b.InternalIPAddress, b.ID, mid, nil
		}
		// Loud, with the ACTUAL modelid: if ModelHueBridgePro is ever wrong, this is the
		// breadcrumb that explains why every Pro is being rejected.
		log.Warn("hue bridge pro selection: discovered bridge is not a Hue Bridge Pro", "host", b.InternalIPAddress, "modelid", mid, "want", bridgepro.ModelHueBridgePro)
		seen = append(seen, mid)
	}
	if anyResponded {
		return "", "", "", fmt.Errorf("%w (found modelid %v, want %s)", ErrNoProBridge, seen, bridgepro.ModelHueBridgePro)
	}
	return "", "", "", fmt.Errorf("%w: %d discovered", ErrProModelUnknown, len(bridges))
}

// pinProShell builds the *config.BridgePro shell for a host: it pins the leaf
// certificate (unless skipTLS) and returns the shell ready for Pair. discoveryID
// is the discovery id captured at selection and is carried onto the shell so
// it survives the eventual SetPro (and disambiguates the bridge on later reconnects).
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

// proWatcher keeps the already-paired Hue Bridge Pro reachable. It health-checks
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
	skipTLS    bool
	log        *slog.Logger

	// interval is the health-check cadence. Default 60s; the setup wizard sets a
	// faster cadence (~4s) so steps 3 and 5 (Pro power-cycle) feel responsive.
	interval time.Duration
	// onReachable, if set, is called every tick with the Pro's current reachability so
	// the setup state machine can drive steps 3 and 5. Called whether or not the value
	// changed (the machine re-evaluates its level-read steps each tick).
	onReachable func(reachable bool)

	// lastDiscover throttles mDNS re-discovery so the fast setup tick doesn't browse the
	// network every few seconds. The reachability tick still health-checks the stored host
	// every tick — a power-cycled Pro usually returns at the SAME IP, detected immediately
	// with no browse — and only falls back to an mDNS re-browse at discoverThrottle cadence
	// when the host genuinely changed. Zero value lets the first attempt through.
	lastDiscover time.Time

	healthCheck      func(*config.BridgePro) error
	discover         func() ([]bridgepro.DiscoveredBridge, error)
	fetchFingerprint func(host string) (string, error)
	applyProvider    func(*config.BridgePro)
}

// Minimum spacing between mDNS re-discovery browses in the watcher, independent of its
// health-check cadence. Shorter during setup so the step-5 "Pro back on" transition is
// detected promptly even when the power-cycle gave the Pro a new DHCP IP (a same-IP
// return is caught instantly by the health check, no browse); gentler once committed.
const (
	discoverThrottleSetup  = 15 * time.Second
	discoverThrottleSteady = 60 * time.Second
)

// discoverThrottle returns the current re-discovery spacing (faster while the setup is
// still in progress).
func (w *proWatcher) discoverThrottle() time.Duration {
	if w.cfg.Committed() {
		return discoverThrottleSteady
	}
	return discoverThrottleSetup
}

// newProWatcher constructs a proWatcher with the production seams wired in.
func newProWatcher(cfg *config.Config, clip *clipv1.Server, controlled *bridge.ControlledSet, live *liveColors, stats *proStats, skipTLS bool, log *slog.Logger) *proWatcher {
	w := &proWatcher{
		cfg:        cfg,
		clip:       clip,
		controlled: controlled,
		liveColors: live,
		stats:      stats,
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
	// Browse mDNS for Hue bridges, excluding relume-tv's own announcement (it advertises
	// itself as a Hue bridge to the TV under the same bridge id).
	w.discover = func() ([]bridgepro.DiscoveredBridge, error) {
		return bridgepro.Discover(cfg.Identity.BridgeID())
	}
	w.fetchFingerprint = bridgepro.FetchLeafFingerprint
	w.applyProvider = func(p *config.BridgePro) {
		clip.SetLightProvider(newProvider(bridgepro.New(p), controlled, live, stats, log))
	}
	return w
}

// run drives the health-check/reconnect cycle until ctx is cancelled, looping calling
// tick. The cadence is w.interval during setup (fast, so the step 3/5 power-cycle is
// responsive) but drops to the gentle 60s once the setup is committed — past that there
// is no wizard to drive and the queue-sensitive Pro should not be probed every few
// seconds for the process's whole lifetime.
func (w *proWatcher) run(ctx context.Context) {
	if w.cfg.GetPro() == nil {
		return
	}
	for sleepCtx(ctx, w.checkInterval()) {
		w.tick()
	}
}

// checkInterval is the current health-check cadence: the configured fast setup cadence
// while the setup is still in progress, otherwise the gentle steady-state 60s. The
// default (interval==0) is 60s too.
func (w *proWatcher) checkInterval() time.Duration {
	const steady = 60 * time.Second
	if w.interval <= 0 || w.cfg.Committed() {
		return steady
	}
	return w.interval
}

// tick runs ONE health-check + maybe-reconnect cycle and reports whether it
// reconnected the Pro to a new pairing. It reads the current pairing via
// cfg.GetPro so a reconnect from a previous tick flows forward automatically.
func (w *proWatcher) tick() (reconnected bool) {
	pro := w.cfg.GetPro()
	if pro == nil {
		w.reportReachable(false)
		return false
	}

	err := w.healthCheck(pro)
	if err == nil {
		w.reportReachable(true) // still reachable
		return false
	}
	// M3: a 503 "command queue full" (or a per-attribute domain rejection) means the
	// Pro is reachable but busy. Re-discovering / re-pinning then is harmful churn —
	// log and back off WITHOUT touching the pairing. Only a genuinely unreachable Pro
	// (or any non-queue/non-domain error) proceeds to re-discover.
	if errors.Is(err, bridgepro.ErrQueueFull) || errors.Is(err, bridgepro.ErrDomain) {
		w.log.Warn("hue bridge pro busy (command queue full) — not re-discovering; backing off", "", pro, "err", err)
		w.reportReachable(true) // reachable, just busy
		return false
	}

	// During setup the Pro is deliberately powered off (wizard steps "disconnect" then
	// "turn back on"), so these unreachable/retry messages are not only noise — the
	// alarming "turn it back on" guidance would mislead the user into powering the Pro
	// back on too early, before the TV has paired with relume-tv, breaking the flow.
	// Suppress them to Debug until the setup is committed; the wizard narrates the steps.
	w.retryLog("hue bridge pro not reachable — is it turned off? "+
		"Turn it back on (or check its power/network cable); "+
		"relume-tv can't control the lights until it is back. Retrying.", "", pro, "err", err)

	// Throttle mDNS re-discovery: the health check above already proves the stored host
	// is down and reports it; a power-cycled Pro usually returns at the same IP (caught by
	// the next health check, no browse). Only re-browse at discoverThrottle cadence, so the
	// fast setup tick — which runs while the Pro is intentionally off for minutes — doesn't
	// browse the network every few seconds.
	if !w.lastDiscover.IsZero() && time.Since(w.lastDiscover) < w.discoverThrottle() {
		w.reportReachable(false)
		return false
	}
	w.lastDiscover = time.Now()

	host, discoveryID, derr := resolveProHost(pro.DiscoveryID, w.discover)
	if derr != nil || host == "" {
		if pro.DiscoveryID != "" {
			w.retryLog("hue bridge pro reconnect: stored bridge not found via discovery; will retry", "discoveryId", pro.DiscoveryID, "err", derr)
		} else {
			w.retryLog("hue bridge pro reconnect: not found via discovery; will retry", "err", derr)
		}
		w.reportReachable(false)
		return false
	}

	certSHA := pro.CertSHA256
	if !w.skipTLS && !pro.SkipTLSVerify {
		fp, ferr := w.fetchFingerprint(host)
		if ferr != nil {
			w.retryLog("hue bridge pro reconnect: cert fetch failed; will retry", "host", host, "err", ferr)
			w.reportReachable(false)
			return false
		}
		certSHA = fp
	}

	updated := reconnectProConfig(pro, host, certSHA, w.skipTLS)
	// A discovered id refreshes the stored one; otherwise reconnectProConfig already
	// carried the old DiscoveryID forward.
	if discoveryID != "" {
		updated.DiscoveryID = discoveryID
	}
	if verr := w.healthCheck(updated); verr != nil {
		w.retryLog("hue bridge pro reconnect: still unreachable", "host", host, "err", verr)
		w.reportReachable(false)
		return false
	}
	if serr := w.cfg.SetPro(updated); serr != nil {
		w.log.Error("persisting reconnected hue bridge pro", "err", serr)
		w.reportReachable(false)
		return false
	}
	w.applyProvider(updated)
	w.log.Info("hue bridge pro reconnected", "", updated)
	// The reconnect found the Pro at a (possibly new DHCP) IP and validated it — this is
	// exactly the step-5 "Pro back on" signal, including after a power-cycle changed the IP.
	w.reportReachable(true)
	return true
}

// reportReachable forwards the Pro's current reachability to the setup state machine
// (no-op when no callback is wired, e.g. for the steady-state watcher after setup).
func (w *proWatcher) reportReachable(reachable bool) {
	if w.onReachable != nil {
		w.onReachable(reachable)
	}
}

// retryLog logs a watcher unreachable/retry message at WARN once the setup is committed
// (steady state — a Pro that goes away then is a real problem), but only at DEBUG while
// the setup is still in progress: there the Pro is intentionally powered off as a wizard
// step, so a loud "turn it back on" would mislead the user into breaking the flow.
func (w *proWatcher) retryLog(msg string, args ...any) {
	if w.cfg.Committed() {
		w.log.Warn(msg, args...)
		return
	}
	w.log.Debug(msg, args...)
}
