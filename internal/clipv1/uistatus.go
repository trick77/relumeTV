package clipv1

// UIStatus reports the current control state for the web UI (read-only).
func (s *Server) UIStatus() (mode string, dtlsUp, fallback bool) {
	fb := s.stream.inFallback()
	dtlsUp = s.stream.isUp()
	switch {
	case s.EntertainmentMode && !fb:
		mode = "entertainment"
	default:
		mode = "rest"
	}
	fallback = s.EntertainmentMode && fb
	return mode, dtlsUp, fallback
}

// PendingTVPairing reports whether a TV pairing attempt is currently inside its
// auto-accept window and not yet paired (read-only, for the web UI).
func (s *Server) PendingTVPairing() bool {
	if len(s.cfg.PairedDeviceTypes()) > 0 {
		return false
	}
	return s.pairing.pending()
}

// LightsV1Snapshot returns the v1 light map for the web UI, or ok=false when no
// provider/Pro is wired yet or the read fails (read-only).
func (s *Server) LightsV1Snapshot() (map[string]any, bool) {
	p := s.lightProvider()
	if p == nil {
		return nil, false
	}
	m, err := p.LightsV1()
	if err != nil {
		return nil, false
	}
	return m, true
}

// UUIDForV1 maps a v1 light id to its Hue Bridge Pro UUID for the web UI. The
// concrete provider implements this; the clipv1 LightProvider interface does
// not, so this best-effort type-asserts and returns ok=false otherwise.
func (s *Server) UUIDForV1(v1id string) (string, bool) {
	p := s.lightProvider()
	if p == nil {
		return "", false
	}
	if u, ok := p.(interface {
		UUIDForV1(string) (string, bool)
	}); ok {
		return u.UUIDForV1(v1id)
	}
	return "", false
}

// V1ForUUID maps a Hue Bridge Pro UUID back to its v1 light id — the inverse of
// UUIDForV1. Used to intersect a flash target (Pro UUIDs) with the TV's current
// Ambilight membership (keyed by v1 id). Best-effort type-assert like UUIDForV1.
func (s *Server) V1ForUUID(uuid string) (string, bool) {
	p := s.lightProvider()
	if p == nil {
		return "", false
	}
	if u, ok := p.(interface {
		V1ForUUID(string) (string, bool)
	}); ok {
		return u.V1ForUUID(uuid)
	}
	return "", false
}
