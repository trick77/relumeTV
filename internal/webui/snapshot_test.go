package webui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	driven []string
	live   map[string]LiveColor
}

func (f fakeSource) Version() string      { return "1.4.2" }
func (f fakeSource) StartedAt() time.Time { return time.Unix(1000, 0).UTC() }
func (f fakeSource) ProInfo() (bool, string, string, bool) {
	return true, "Living Room Pro", "192.168.178.40", true
}
func (f fakeSource) TVClients() []string            { return []string{"Ambilight#65OLED806"} }
func (f fakeSource) ModeInfo() (string, bool, bool) { return "entertainment", true, false }
func (f fakeSource) BridgeName() string             { return "Philips Hue - 2C4D54" }
func (f fakeSource) PendingTVPairing() bool         { return false }
func (f fakeSource) LastActivity() time.Time        { return time.Time{} }
func (f fakeSource) LightsV1() (map[string]any, bool) {
	return map[string]any{
		"1": map[string]any{
			"name":  "Sofa",
			"state": map[string]any{"on": true, "bri": float64(200), "xy": []any{0.5, 0.4}},
		},
	}, true
}
func (f fakeSource) UUIDForV1(v1id string) (string, bool) { return "uuid-" + v1id, true }
func (f fakeSource) DrivenUUIDs() []string                { return f.driven }
func (f fakeSource) LiveColors() map[string]LiveColor     { return f.live }
func (f fakeSource) Active() bool                         { return true }

func TestBuildSnapshot_MapsLightsAndDriven(t *testing.T) {
	s := BuildSnapshot(fakeSource{driven: []string{"uuid-1"}})
	if !s.ProPaired || s.ProName != "Living Room Pro" || !s.CertPinned {
		t.Fatalf("pro fields = %+v", s)
	}
	if s.Health != "streaming-pro" {
		t.Fatalf("health = %q, want streaming-pro", s.Health)
	}
	if len(s.Lights) != 1 {
		t.Fatalf("lights = %+v", s.Lights)
	}
	l := s.Lights[0]
	if l.Name != "Sofa" || !l.On || l.Bri != 200 || l.X != 0.5 || !l.Driven {
		t.Fatalf("light = %+v", l)
	}
	if s.LastActivity != "" {
		t.Fatalf("zero time should render empty, got %q", s.LastActivity)
	}
}

// In the default DTLS-passthrough mode the ControlledSet is never fed (Seen runs
// only on the REST path), so DrivenUUIDs is empty. A light's presence in LiveColors
// is then the only per-light signal: it must both override the swatch colour and
// mark the light driven.
func TestBuildSnapshot_LiveColorOverridesAndDrivesLight(t *testing.T) {
	src := fakeSource{
		driven: nil, // pure DTLS: nothing in the ControlledSet
		live:   map[string]LiveColor{"1": {X: 0.1, Y: 0.2, Bri: 99, On: true}},
	}
	s := BuildSnapshot(src)
	l := s.Lights[0]
	if !l.Driven {
		t.Fatalf("light should be driven from LiveColors presence, got %+v", l)
	}
	if l.X != 0.1 || l.Y != 0.2 || l.Bri != 99 {
		t.Fatalf("live colour should override Pro REST state, got %+v", l)
	}
}

// emptySource models a fresh, unpaired install: no provider, no TV clients.
type emptySource struct{}

func (emptySource) Version() string                       { return "dev" }
func (emptySource) StartedAt() time.Time                  { return time.Time{} }
func (emptySource) ProInfo() (bool, string, string, bool) { return false, "", "", false }
func (emptySource) TVClients() []string                   { return nil }
func (emptySource) ModeInfo() (string, bool, bool)        { return "rest", false, false }
func (emptySource) BridgeName() string                    { return "Philips Hue - ABCDEF" }
func (emptySource) PendingTVPairing() bool                { return false }
func (emptySource) LastActivity() time.Time               { return time.Time{} }
func (emptySource) LightsV1() (map[string]any, bool)      { return nil, false }
func (emptySource) UUIDForV1(string) (string, bool)       { return "", false }
func (emptySource) DrivenUUIDs() []string                 { return nil }
func (emptySource) LiveColors() map[string]LiveColor      { return nil }
func (emptySource) Active() bool                          { return false }

func TestBuildSnapshot_EmptyArraysNotNil(t *testing.T) {
	s := BuildSnapshot(emptySource{})
	// Lights and TVClients must marshal as [] (never null), or the frontend's
	// .length access crashes the setup wizard on a fresh install.
	if s.Lights == nil {
		t.Fatal("Lights is nil — would serialize to JSON null and crash the wizard")
	}
	if s.TVClients == nil {
		t.Fatal("TVClients is nil — would serialize to JSON null")
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"lights":[]`) || !strings.Contains(string(b), `"tvClients":[]`) {
		t.Fatalf("expected empty arrays in JSON, got %s", b)
	}
}

// idleSource: Pro paired, TV paired, but the TV is not currently driving.
type idleSource struct{ fakeSource }

func (idleSource) ModeInfo() (string, bool, bool) { return "rest", false, false }
func (idleSource) Active() bool                   { return false }

func TestBuildSnapshot_IdleWhenTVNotDriving(t *testing.T) {
	s := BuildSnapshot(idleSource{})
	if s.Health != "idle" {
		t.Fatalf("health = %q, want idle (TV paired but not driving)", s.Health)
	}
}

type restFallbackSource struct{ fakeSource }

func (restFallbackSource) ModeInfo() (string, bool, bool) { return "entertainment", false, true }

// B: the TV activated a stream but DTLS to the Pro failed, so relume reverted to
// REST. This is a degraded fallback and must read distinctly from a plain
// REST-follow so the UI can flag it.
func TestBuildSnapshot_HealthDegradesToFallback(t *testing.T) {
	s := BuildSnapshot(restFallbackSource{})
	if s.Health != "entertainment-fallback" {
		t.Fatalf("health = %q, want entertainment-fallback", s.Health)
	}
	if !s.Fallback {
		t.Fatalf("expected fallback=true, got %+v", s)
	}
}

type entertainmentRESTSource struct{ fakeSource }

func (entertainmentRESTSource) ModeInfo() (string, bool, bool) {
	return "entertainment", false, false
}

// C: entertainment mode configured, the TV is driving, but it never opened a
// DTLS stream (no fallback). Surfaced as the shared "active-rest" state and must
// NOT imply a fallback that never happened.
func TestBuildSnapshot_EntertainmentRESTWhenTVNotStreaming(t *testing.T) {
	s := BuildSnapshot(entertainmentRESTSource{})
	if s.Health != "active-rest" {
		t.Fatalf("health = %q, want active-rest", s.Health)
	}
	if s.Fallback {
		t.Fatalf("expected fallback=false (no fallback occurred), got %+v", s)
	}
}

// REST mode: REST is the intended, configured path — not a degradation. Shares the
// "active-rest" health state with entertainment-configured-but-not-streaming.
type restModeSource struct{ fakeSource }

func (restModeSource) ModeInfo() (string, bool, bool) { return "rest", false, false }

func TestBuildSnapshot_RESTModeIsActiveRest(t *testing.T) {
	s := BuildSnapshot(restModeSource{})
	if s.Health != "active-rest" {
		t.Fatalf("health = %q, want active-rest", s.Health)
	}
}
