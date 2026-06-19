package main

import (
	"time"

	"github.com/trick77/relume/internal/clipv1"
	"github.com/trick77/relume/internal/config"
	"github.com/trick77/relume/internal/webui"
)

// uiSource adapts relume's live state to webui.StateSource. It is read-only and
// exposes no secrets (app/client keys, cert fingerprint never leave the core).
type uiSource struct {
	cfg        *config.Config
	clip       *clipv1.Server
	liveColors *liveColors
	frameStats *frameStats
	advName    string
	version    string
	started    time.Time
	// activeWindow is how recently the TV must have driven the lights to count as
	// "active" in the UI. Mirrors the idle-off window so the UI reports "idle" once
	// relume itself considers the TV gone.
	activeWindow time.Duration
}

func (u *uiSource) Version() string      { return u.version }
func (u *uiSource) StartedAt() time.Time { return u.started }

func (u *uiSource) ProInfo() (bool, string, string, bool) {
	p := u.cfg.GetPro()
	if p == nil {
		return false, "", "", false
	}
	return true, p.Name, p.Host, p.CertSHA256 != ""
}

func (u *uiSource) TVClients() []string                { return u.cfg.PairedDeviceTypes() }
func (u *uiSource) ModeInfo() (string, bool, bool)     { return u.clip.UIStatus() }
func (u *uiSource) BridgeName() string                 { return u.advName }
func (u *uiSource) PendingTVPairing() bool             { return u.clip.PendingTVPairing() }
func (u *uiSource) LastActivity() time.Time            { return u.clip.LastActivity() }
func (u *uiSource) LightsV1() (map[string]any, bool)   { return u.clip.LightsV1Snapshot() }

// DrivenV1IDs returns the v1 light ids the TV is driving right now (seen within the
// liveColors freshness window). Empties soon after the stream stops, unlike the
// sticky ControlledSet — so the UI count and the manual flash reflect the live set.
func (u *uiSource) DrivenV1IDs() []string { return u.liveColors.DrivenV1IDs() }

func (u *uiSource) LiveColors() map[string]webui.LiveColor { return u.liveColors.Snapshot() }

func (u *uiSource) StreamFPS() int { return u.frameStats.FPS() }

func (u *uiSource) Active() bool {
	last := u.clip.LastActivity()
	if last.IsZero() {
		return false
	}
	return time.Since(last) < u.activeWindow
}
