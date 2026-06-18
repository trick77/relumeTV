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
// relume actually sees the values flow TV→Pro (the REST forward and the DTLS
// passthrough). The UI uses it to render the live swatch colour instead of the
// Bridge Pro's REST light state, which the DTLS passthrough never updates.
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
	CertPinned   bool        `json:"certPinned"`
	TVClients    []string    `json:"tvClients"`
	Mode         string      `json:"mode"`
	DTLSStreamUp bool        `json:"dtlsStreamUp"`
	Fallback     bool        `json:"fallback"`
	BridgeName   string      `json:"bridgeName"`
	PendingTV    bool        `json:"pendingTV"`
	LastActivity string      `json:"lastActivity"`
	Lights       []LightView `json:"lights"`
	Health       string      `json:"health"`
}

// StateSource exposes relume's live state to the snapshot builder without
// coupling the UI to the control internals. Implemented by cmd/relume.
type StateSource interface {
	Version() string
	StartedAt() time.Time
	ProInfo() (paired bool, name, host string, certPinned bool)
	TVClients() []string
	ModeInfo() (mode string, dtlsUp, fallback bool)
	BridgeName() string
	PendingTVPairing() bool
	LastActivity() time.Time
	LightsV1() (map[string]any, bool)
	UUIDForV1(v1id string) (string, bool)
	DrivenUUIDs() []string
	// LiveColors maps v1 light id → the latest colour the TV streamed for it.
	// A light present here is being driven by the TV (the only point the DTLS
	// passthrough exposes per-light state to the UI), so the snapshot both
	// overrides the swatch colour and marks the light driven from this set.
	LiveColors() map[string]LiveColor
	// Active reports whether the TV is currently driving the lights (it has
	// written/streamed within the idle window). False when the TV is off or idle.
	Active() bool
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// BuildSnapshot assembles a Snapshot from the source. It reads no secrets.
func BuildSnapshot(src StateSource) Snapshot {
	paired, name, host, pinned := src.ProInfo()
	mode, dtlsUp, fallback := src.ModeInfo()
	tv := src.TVClients()
	// Always emit arrays, never null: the frontend reads .length on these, so a
	// nil slice (→ JSON null) would crash the setup wizard on a fresh install.
	if tv == nil {
		tv = []string{}
	}

	s := Snapshot{
		Version:      src.Version(),
		StartedAt:    rfc3339(src.StartedAt()),
		ProPaired:    paired,
		ProName:      name,
		ProHost:      host,
		CertPinned:   pinned,
		TVClients:    tv,
		Mode:         mode,
		DTLSStreamUp: dtlsUp,
		Fallback:     fallback,
		BridgeName:   src.BridgeName(),
		PendingTV:    src.PendingTVPairing(),
		LastActivity: rfc3339(src.LastActivity()),
		Lights:       []LightView{},
	}

	switch {
	case !paired:
		s.Health = "unpaired-pro"
	case len(tv) == 0:
		s.Health = "no-tv"
	case !src.Active():
		// TV is paired but not currently driving (off / Ambilight idle). Don't claim
		// "Active" — relume is just standing by.
		s.Health = "idle"
	case mode == "entertainment" && dtlsUp && !fallback:
		s.Health = "streaming-pro"
	case mode == "entertainment" && fallback:
		// B: the TV activated a stream but relume could not drive the Pro over DTLS,
		// so it reverted to REST-follow. A degraded state worth flagging distinctly.
		s.Health = "entertainment-fallback"
	default:
		// Driving the lights over per-light REST writes. Two non-degraded ways to
		// land here, both surfaced as a single "Active": entertainment mode is
		// configured but the TV never opened a DTLS stream (nothing failed — it just
		// isn't streaming), or relume is in plain REST mode where REST is the intended
		// path. Neither is a fallback, so they share one health state.
		s.Health = "active-rest"
	}

	if lv1, ok := src.LightsV1(); ok {
		driven := map[string]struct{}{}
		for _, u := range src.DrivenUUIDs() {
			driven[u] = struct{}{}
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
			if uuid, ok := src.UUIDForV1(id); ok {
				if _, d := driven[uuid]; d {
					lv.Driven = true
				}
			}
			// Live colour overrides the Pro's REST light state (stale during DTLS
			// passthrough) and marks the light driven: a streamed colour is the only
			// per-light signal the DTLS path surfaces to the UI. Only override the
			// colour fields that are actually present, so an xy-less write (e.g. a bare
			// on/off REST write) does not blank the swatch to black.
			if lc, ok := live[id]; ok {
				lv.On = lc.On
				lv.Driven = true
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
