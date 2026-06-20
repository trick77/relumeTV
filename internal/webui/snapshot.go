package webui

import (
	"sort"
	"strconv"
	"time"
)

// LightView is one light as the UI renders it: name, on/off, brightness, CIE xy
// colour, and whether the TV is currently driving it.
type LightView struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	On     bool    `json:"on"`
	Bri    int     `json:"bri"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Driven bool    `json:"driven"`
}

// LiveColor is the most recent colour the TV pushed for one light, captured where
// relumeTV actually sees the values flow TV→Pro (the REST forward and the DTLS
// passthrough). The UI uses it to render the live swatch colour instead of the
// Hue Bridge Pro's REST light state, which the DTLS passthrough never updates.
type LiveColor struct {
	X   float64
	Y   float64
	Bri int
	On  bool
}

// Snapshot is the complete UI-facing state. It is built solely from non-secret
// sources — app keys, client keys and cert fingerprints never appear here.
type Snapshot struct {
	Version      string      `json:"version"`
	StartedAt    string      `json:"startedAt"`
	ProPaired    bool        `json:"proPaired"`
	ProName      string      `json:"proName"`
	ProHost      string      `json:"proHost"`
	ProBridgeID  string      `json:"proBridgeId,omitempty"`
	CertPinned   bool        `json:"certPinned"`
	TVClients    []string    `json:"tvClients"`
	Mode         string      `json:"mode"`
	DTLSStreamUp bool        `json:"dtlsStreamUp"`
	Fallback     bool        `json:"fallback"`
	BridgeName   string      `json:"bridgeName"`
	LastActivity string      `json:"lastActivity"`
	Lights       []LightView `json:"lights"`
	Health       string      `json:"health"`
	// StreamFPS is the live entertainment-stream frame rate (frames/s the TV is
	// pushing over DTLS). Non-zero only while streaming to the Pro; 0 otherwise.
	StreamFPS int `json:"streamFps"`
	// ProSendFPS is relumeTV's outgoing DTLS frame rate to the Pro (frames/s, the 50 Hz
	// sendLoop). The upsampled counterpart to StreamFPS; non-zero only in DTLS mode.
	ProSendFPS int `json:"proSendFps,omitempty"`
	// ProWriteRate is relumeTV's outgoing REST write rate to the Pro (writes/s, per
	// light, coalesced). The REST-path counterpart to ProSendFPS; non-zero only when
	// driving the Pro over REST (fallback or plain REST-follow).
	ProWriteRate int `json:"proWriteRate,omitempty"`
	// CoalesceRate is the rate (per second) of frames the optimistic REST path
	// dropped because the Hue Bridge Pro could not keep up. This is healthy backpressure
	// (the Pro spared a write it could not accept), NOT an error — the UI must not
	// render it as a failure. Non-zero only on the REST path.
	CoalesceRate int `json:"coalesceRate,omitempty"`
	// ForwardErrors is the cumulative count of failed REST writes to the Pro since
	// start (down Pro / 503 overflow) — the real failure signal, distinct from
	// CoalesceRate. A count, not a rate: forward errors are rare, so a rate would
	// mostly read 0 and hide that errors happened earlier.
	ForwardErrors int `json:"forwardErrors"`
	// LastForwardErr is the RFC3339 time of the most recent failed Pro write (empty
	// if none). The UI keys the amber "N err" warning off its age, decaying it back
	// to the healthy state once writes have been succeeding again for a while — so a
	// long-resolved fault does not leave a permanent warning.
	LastForwardErr string `json:"lastForwardErr,omitempty"`
	// SmoothingTauMs is the DTLS-path easing time constant in ms (a fixed config knob).
	// The Stream card's jitter tooltip reads it so the explanation stays accurate.
	SmoothingTauMs int `json:"smoothingTauMs,omitempty"`
	// JitterInBri / JitterSentBri are the latest per-window max brightness jump (16-bit,
	// 0–65535) on the incoming TV stream vs relumeTV's smoothed sent stream. The UI shows
	// the reduction (1 − sent/in). Both 0 when not streaming to the Pro over DTLS, so the
	// card renders a longdash rather than a stale figure.
	JitterInBri   int `json:"jitterInBri,omitempty"`
	JitterSentBri int `json:"jitterSentBri,omitempty"`
}

// StateSource exposes relumeTV's live state to the snapshot builder without
// coupling the UI to the control internals. Implemented by cmd/relumetv.
type StateSource interface {
	Version() string
	StartedAt() time.Time
	ProInfo() (paired bool, name, host, bridgeID string, certPinned bool)
	TVClients() []string
	ModeInfo() (mode string, dtlsUp, fallback bool)
	BridgeName() string
	LastActivity() time.Time
	LightsV1() (map[string]any, bool)
	// DrivenV1IDs lists the v1 light ids the TV is driving right now (a freshness
	// window, not a sticky set): the single source for the driven count and the
	// per-light "driven" marking. Empties soon after the TV stops streaming.
	DrivenV1IDs() []string
	// LiveColors maps v1 light id → the latest colour the TV streamed for it. Used
	// only to override the swatch colour (the Pro's REST light state is stale during
	// DTLS passthrough); it does NOT decide "driven" — DrivenV1IDs does — so a light
	// keeps its last colour after a stream stops without staying marked driven.
	LiveColors() map[string]LiveColor
	// Active reports whether the TV is currently driving the lights (it has
	// written/streamed within the idle window). False when the TV is off or idle.
	Active() bool
	// StreamFPS is the live entertainment-stream frame rate (TV→relumeTV over DTLS).
	// 0 when no DTLS stream is running.
	StreamFPS() int
	// ProSendFPS is relumeTV's outgoing DTLS frame rate to the Pro (frames/s). 0 unless
	// streaming to the Pro over DTLS.
	ProSendFPS() int
	// ProWriteRate is relumeTV's outgoing REST write rate to the Pro (writes/s). 0
	// unless driving the Pro over REST.
	ProWriteRate() int
	// CoalesceRate is the rate (per second) of frames the optimistic REST path
	// dropped because the Pro could not keep up — healthy backpressure, not an error.
	CoalesceRate() int
	// ForwardErrors is the cumulative count of failed REST writes to the Pro since
	// start — the real failure signal (down Pro / 503 overflow).
	ForwardErrors() int
	// LastForwardErr is the time of the most recent failed Pro write (zero if none),
	// so the UI can decay the error warning once writes are succeeding again.
	LastForwardErr() time.Time
	// SmoothingTauMs is the DTLS-path easing time constant (ms), for the Stream card's
	// jitter tooltip.
	SmoothingTauMs() int
	// Jitter returns the latest incoming vs smoothed-sent brightness jump and whether
	// the pair is fresh. ok is false when not streaming to the Pro over DTLS, so the UI
	// shows a longdash instead of a stale reduction.
	Jitter() (inBri, sentBri int, ok bool)
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// BuildSnapshot assembles a Snapshot from the source. It reads no secrets.
func BuildSnapshot(src StateSource) Snapshot {
	paired, name, host, bridgeID, pinned := src.ProInfo()
	mode, dtlsUp, fallback := src.ModeInfo()
	tv := src.TVClients()
	// Always emit arrays, never null: the frontend reads .length on these, so a
	// nil slice (→ JSON null) would crash the setup wizard on a fresh install.
	if tv == nil {
		tv = []string{}
	}

	s := Snapshot{
		Version:        src.Version(),
		StartedAt:      rfc3339(src.StartedAt()),
		ProPaired:      paired,
		ProName:        name,
		ProHost:        host,
		ProBridgeID:    bridgeID,
		CertPinned:     pinned,
		TVClients:      tv,
		Mode:           mode,
		DTLSStreamUp:   dtlsUp,
		Fallback:       fallback,
		BridgeName:     src.BridgeName(),
		LastActivity:   rfc3339(src.LastActivity()),
		Lights:         []LightView{},
		StreamFPS:      src.StreamFPS(),
		ProSendFPS:     src.ProSendFPS(),
		ProWriteRate:   src.ProWriteRate(),
		CoalesceRate:   src.CoalesceRate(),
		ForwardErrors:  src.ForwardErrors(),
		LastForwardErr: rfc3339(src.LastForwardErr()),
		SmoothingTauMs: src.SmoothingTauMs(),
	}
	if inBri, sentBri, ok := src.Jitter(); ok {
		s.JitterInBri = inBri
		s.JitterSentBri = sentBri
	}

	switch {
	case !paired:
		s.Health = "unpaired-pro"
	case len(tv) == 0:
		s.Health = "no-tv"
	case !src.Active():
		// TV is paired but not currently driving (off / Ambilight idle). Don't claim
		// "Active" — relumeTV is just standing by.
		s.Health = "idle"
	case mode == "entertainment" && dtlsUp && !fallback:
		s.Health = "streaming-pro"
	case mode == "entertainment" && fallback:
		// B: the TV activated a stream but relumeTV could not drive the Pro over DTLS,
		// so it reverted to REST-follow. A degraded state worth flagging distinctly.
		s.Health = "entertainment-fallback"
	default:
		// Driving the lights over per-light REST writes. Two non-degraded ways to
		// land here, both surfaced as a single "Active": entertainment mode is
		// configured but the TV never opened a DTLS stream (nothing failed — it just
		// isn't streaming), or relumeTV is in plain REST mode where REST is the intended
		// path. Neither is a fallback, so they share one health state.
		s.Health = "active-rest"
	}

	if lv1, ok := src.LightsV1(); ok {
		driven := map[string]struct{}{}
		for _, id := range src.DrivenV1IDs() {
			driven[id] = struct{}{}
		}
		live := src.LiveColors()
		for id, raw := range lv1 {
			m, _ := raw.(map[string]any)
			st, _ := m["state"].(map[string]any)
			lv := LightView{ID: id}
			if n, ok := m["name"].(string); ok {
				lv.Name = n
			}
			if st != nil {
				lv.On, _ = st["on"].(bool)
				if b, ok := st["bri"].(float64); ok {
					lv.Bri = int(b)
				}
				if xy, ok := st["xy"].([]any); ok && len(xy) == 2 {
					lv.X, _ = xy[0].(float64)
					lv.Y, _ = xy[1].(float64)
				}
			}
			if _, d := driven[id]; d {
				lv.Driven = true
			}
			// Live colour overrides the Pro's REST light state (stale during DTLS
			// passthrough). This is purely a swatch-colour override and does NOT mark
			// the light driven — that is DrivenV1IDs' job (a windowed signal), so a
			// light keeps its last colour after a stop without staying "driven". Only
			// override the colour fields actually present, so an xy-less write (e.g. a
			// bare on/off REST write) does not blank the swatch to black.
			if lc, ok := live[id]; ok {
				lv.On = lc.On
				if lc.Bri > 0 {
					lv.Bri = lc.Bri
				}
				if lc.X != 0 || lc.Y != 0 {
					lv.X = lc.X
					lv.Y = lc.Y
				}
			}
			s.Lights = append(s.Lights, lv)
		}
		// lv1 is a map, so iteration order is non-deterministic. Sort by the
		// numeric v1 ID for a stable, predictable light order in the UI.
		sort.Slice(s.Lights, func(i, j int) bool {
			ai, _ := strconv.Atoi(s.Lights[i].ID)
			aj, _ := strconv.Atoi(s.Lights[j].ID)
			return ai < aj
		})
	}
	return s
}
