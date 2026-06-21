package main

import (
	"time"

	"github.com/trick77/relume-tv/internal/clipv1"
	"github.com/trick77/relume-tv/internal/config"
	"github.com/trick77/relume-tv/internal/webui"
)

// uiSource adapts relume-tv's live state to webui.StateSource. It is read-only and
// exposes no secrets (app/client keys, cert fingerprint never leave the core).
type uiSource struct {
	cfg           *config.Config
	clip          *clipv1.Server
	liveColors    *liveColors
	frameStats    *frameStats
	proSendStats  *frameStats
	restRecvStats *frameStats
	proStats      *proStats
	jitterStats   *jitterStats
	setup         *setupStatus
	advName       string
	version       string
	started       time.Time
	smoothTauMs   int
}

func (u *uiSource) Version() string      { return u.version }
func (u *uiSource) StartedAt() time.Time { return u.started }

func (u *uiSource) ProInfo() (bool, string, string, string, bool) {
	p := u.cfg.GetPro()
	if p == nil {
		return false, "", "", "", false
	}
	return true, p.Name, p.Host, p.BridgeID, p.CertSHA256 != ""
}

func (u *uiSource) TVClients() []string              { return u.cfg.PairedDeviceTypes() }
func (u *uiSource) ModeInfo() (string, bool, bool)   { return u.clip.UIStatus() }
func (u *uiSource) BridgeName() string               { return u.advName }
func (u *uiSource) LastActivity() time.Time          { return u.clip.LastActivity() }
func (u *uiSource) LightsV1() (map[string]any, bool) { return u.clip.LightsV1Snapshot() }

// DrivenV1IDs returns the v1 light ids the TV is driving right now (seen within the
// liveColors freshness window). Empties soon after the stream stops, unlike the
// sticky ControlledSet — so the UI count and the manual flash reflect the live set.
func (u *uiSource) DrivenV1IDs() []string { return u.liveColors.DrivenV1IDs() }

func (u *uiSource) LiveColors() map[string]webui.LiveColor { return u.liveColors.Snapshot() }

func (u *uiSource) StreamFPS() int    { return u.frameStats.FPS() }
func (u *uiSource) ProSendFPS() int   { return u.proSendStats.FPS() }
func (u *uiSource) ProWriteRate() int { return u.proStats.writes.FPS() }

// RestRecvRate is the rate (per second) of inbound REST control calls the TV sends relume-tv
// (per-light state PUTs + group-action PUTs) — the REST-path counterpart to StreamFPS. 0
// unless the TV is driving over REST.
func (u *uiSource) RestRecvRate() int { return u.restRecvStats.FPS() }

// CoalesceRate is the rate (per second) of frames the optimistic REST path dropped
// because the Hue Bridge Pro could not keep up — healthy backpressure, not an error.
func (u *uiSource) CoalesceRate() int { return u.proStats.coalesces.FPS() }

// ForwardErrors is the cumulative count of failed REST writes to the Pro since
// start — the real failure signal (down Pro / 503 overflow).
func (u *uiSource) ForwardErrors() int { return int(u.proStats.fwdErrs.Load()) }

// LastForwardErr is the time of the most recent failed REST write (zero if none),
// letting the UI decay the error warning once writes have been succeeding again.
func (u *uiSource) LastForwardErr() time.Time { return u.proStats.LastForwardErr() }

// SmoothingTauMs is the configured DTLS-path easing time constant in milliseconds
// (0 = smoothing off) — surfaced so the Stream card's tooltip can explain the jitter
// reduction without hardcoding it.
func (u *uiSource) SmoothingTauMs() int { return u.smoothTauMs }

// Jitter returns the latest incoming vs smoothed-sent brightness jump and whether the
// pair is fresh (false → UI longdash, i.e. not streaming to the Pro over DTLS).
func (u *uiSource) Jitter() (inBri, sentBri int, ok bool) { return u.jitterStats.Reduction() }

// SetupComplete is the steady-state dashboard gate: the config has been committed AND
// a Pro is paired AND a TV is paired. Deliberately NOT the live activity signal — once
// the setup is committed, an idle/off TV must keep showing the dashboard, never flip
// back to the wizard (the commit, triggered by the first TV activity, is the one-shot
// transition; this is the persisted steady state).
func (u *uiSource) SetupComplete() bool {
	return u.cfg.Committed() && u.cfg.GetPro() != nil && len(u.cfg.PairedDeviceTypes()) > 0
}

// CurrentStep is the active wizard step from the backend state machine.
func (u *uiSource) CurrentStep() int { return u.setup.CurrentStep() }

// FirstRun reports whether this process began a fresh setup (no config file existed).
func (u *uiSource) FirstRun() bool { return u.cfg.FirstRun() }

// SetupInfo bundles the discovery preconditions and live setup signals for the wizard.
func (u *uiSource) SetupInfo() (discoveredHost string, bridgeIsPro, discoveryOK, proReachable, tvDescriptorSeen bool, precondMsg string) {
	discoveryOK, discoveredHost, bridgeIsPro, precondMsg = u.setup.Precond()
	proReachable = u.setup.ProReachable()
	tvDescriptorSeen = u.setup.TVDescriptorSeen()
	return discoveredHost, bridgeIsPro, discoveryOK, proReachable, tvDescriptorSeen, precondMsg
}

// Active reports whether the TV is currently driving any light — tied to the same
// 2s freshness window as the driven count, so the header "Active/Idle" state, the
// Lights "driven" count and the flash button stay consistent (no window where the
// header says Active while the count is 0).
func (u *uiSource) Active() bool {
	return len(u.liveColors.DrivenV1IDs()) > 0
}
