package webui

import "time"

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
	case mode == "entertainment":
		// C: entertainment mode is configured but the TV never opened a DTLS stream —
		// it is driving via per-light REST writes. This is NOT a fallback (nothing
		// failed); the TV simply isn't streaming entertainment. Kept distinct from
		// "following-rest" so the UI doesn't read as a contradiction ("entertainment"
		// next to "REST-follow") and doesn't imply a fallback that never happened.
		s.Health = "entertainment-rest"
	default:
		// REST mode: REST-follow is the intended, configured path.
		s.Health = "following-rest"
	}

	if lv1, ok := src.LightsV1(); ok {
		driven := map[string]struct{}{}
		for _, u := range src.DrivenUUIDs() {
			driven[u] = struct{}{}
		}
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
			s.Lights = append(s.Lights, lv)
		}
	}
	return s
}
