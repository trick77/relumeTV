package webui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	driven []string
}

func (f fakeSource) Version() string     { return "1.4.2" }
func (f fakeSource) StartedAt() time.Time { return time.Unix(1000, 0).UTC() }
func (f fakeSource) ProInfo() (bool, string, string, bool) {
	return true, "Living Room Pro", "192.168.178.40", true
}
func (f fakeSource) TVClients() []string           { return []string{"Ambilight#65OLED806"} }
func (f fakeSource) ModeInfo() (string, bool, bool) { return "entertainment", true, false }
func (f fakeSource) BridgeName() string             { return "Philips Hue - 2C4D54" }
func (f fakeSource) PendingTVPairing() bool          { return false }
func (f fakeSource) LastActivity() time.Time         { return time.Time{} }
func (f fakeSource) LightsV1() (map[string]any, bool) {
	return map[string]any{
		"1": map[string]any{
			"name":  "Sofa",
			"state": map[string]any{"on": true, "bri": float64(200), "xy": []any{0.5, 0.4}},
		},
	}, true
}
func (f fakeSource) UUIDForV1(v1id string) (string, bool) { return "uuid-" + v1id, true }
func (f fakeSource) DrivenUUIDs() []string               { return f.driven }

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

type restFallbackSource struct{ fakeSource }

func (restFallbackSource) ModeInfo() (string, bool, bool) { return "entertainment", false, true }

func TestBuildSnapshot_HealthDegradesToRest(t *testing.T) {
	s := BuildSnapshot(restFallbackSource{})
	if s.Health != "following-rest" {
		t.Fatalf("health = %q, want following-rest", s.Health)
	}
	if !s.Fallback {
		t.Fatalf("expected fallback=true, got %+v", s)
	}
}
