package webui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	driven   []string
	live     map[string]LiveColor
	coalesce int
	fwdErrs  int
	lastErr  time.Time
}

func (f fakeSource) Version() string      { return "1.4.2" }
func (f fakeSource) StartedAt() time.Time { return time.Unix(1000, 0).UTC() }
func (f fakeSource) ProInfo() (bool, string, string, string, bool) {
	return true, "Living Room Pro", "192.168.178.40", "ECB5FAFFFE1A2B3C", true
}
func (f fakeSource) TVClients() []string            { return []string{"Ambilight#65OLED806"} }
func (f fakeSource) ModeInfo() (string, bool, bool) { return "entertainment", true, false }
func (f fakeSource) BridgeName() string             { return "Philips Hue - 2C4D54" }
func (f fakeSource) LastActivity() time.Time        { return time.Time{} }
func (f fakeSource) LightsV1() (map[string]any, bool) {
	return map[string]any{
		"1": map[string]any{
			"name":  "Sofa",
			"state": map[string]any{"on": true, "bri": float64(200), "xy": []any{0.5, 0.4}},
		},
	}, true
}
func (f fakeSource) DrivenV1IDs() []string            { return f.driven }
func (f fakeSource) LiveColors() map[string]LiveColor { return f.live }
func (f fakeSource) Active() bool                     { return true }
func (f fakeSource) StreamFPS() int                   { return 25 }
func (f fakeSource) ProSendFPS() int                  { return 50 }
func (f fakeSource) ProWriteRate() int                { return 0 }
func (f fakeSource) RestRecvRate() int                { return 0 }
func (f fakeSource) CoalesceRate() int                { return f.coalesce }
func (f fakeSource) ForwardErrors() int               { return f.fwdErrs }
func (f fakeSource) LastForwardErr() time.Time        { return f.lastErr }
func (f fakeSource) SmoothingTauMs() int              { return 40 }
func (f fakeSource) Jitter() (int, int, bool)         { return 8000, 1600, true }
func (f fakeSource) SetupComplete() bool              { return true }
func (f fakeSource) CurrentStep() int                 { return stepDoneSnap }
func (f fakeSource) FirstRun() bool                   { return false }
func (f fakeSource) SetupInfo() (string, bool, bool, bool, bool, string) {
	return "10.0.0.5", true, true, true, true, ""
}

// stepDoneSnap mirrors cmd/relume-tv's stepDone sentinel for the webui test fakes
// (kept local so the webui package stays independent of cmd).
const stepDoneSnap = 7

func TestBuildSnapshot_MapsLightsAndDriven(t *testing.T) {
	s := BuildSnapshot(fakeSource{driven: []string{"1"}})
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
	if s.StreamFPS != 25 {
		t.Fatalf("streamFps = %d, want 25 (TV input rate flows through to the snapshot)", s.StreamFPS)
	}
	if s.ProSendFPS != 50 {
		t.Fatalf("proSendFps = %d, want 50 (relume-tv→Pro DTLS send rate flows through)", s.ProSendFPS)
	}
	if s.ProWriteRate != 0 {
		t.Fatalf("proWriteRate = %d, want 0 (no REST writes while streaming DTLS)", s.ProWriteRate)
	}
}

// LiveColors overrides the swatch colour (the Pro's REST state is stale during DTLS
// passthrough) but does NOT mark a light driven — that is DrivenV1IDs' job, a
// windowed signal. A light with a retained colour but no longer in the driven
// window must render its last colour yet not count as driven.
func TestBuildSnapshot_LiveColorOverridesButDoesNotDrive(t *testing.T) {
	src := fakeSource{
		driven: nil, // not in the freshness window any more
		live:   map[string]LiveColor{"1": {X: 0.1, Y: 0.2, Bri: 99, On: true}},
	}
	s := BuildSnapshot(src)
	l := s.Lights[0]
	if l.Driven {
		t.Fatalf("light must NOT be driven from LiveColors presence alone, got %+v", l)
	}
	if l.X != 0.1 || l.Y != 0.2 || l.Bri != 99 {
		t.Fatalf("live colour should still override Pro REST state, got %+v", l)
	}
}

// A light in the driven window is marked driven AND keeps its live colour.
func TestBuildSnapshot_DrivenV1IDMarksLight(t *testing.T) {
	src := fakeSource{
		driven: []string{"1"},
		live:   map[string]LiveColor{"1": {X: 0.1, Y: 0.2, Bri: 99, On: true}},
	}
	l := BuildSnapshot(src).Lights[0]
	if !l.Driven || l.X != 0.1 || l.Bri != 99 {
		t.Fatalf("light should be driven with live colour, got %+v", l)
	}
}

// Backpressure fields flow through to the snapshot: the coalesced drops/s rate and
// the cumulative forward-error count the Backpressure card renders.
func TestBuildSnapshot_BackpressureFieldsFlowThrough(t *testing.T) {
	errAt := time.Unix(1700000000, 0)
	s := BuildSnapshot(fakeSource{driven: []string{"1"}, coalesce: 12, fwdErrs: 3, lastErr: errAt})
	if s.CoalesceRate != 12 {
		t.Fatalf("coalesceRate = %d, want 12 (drops/s flows through)", s.CoalesceRate)
	}
	if s.ForwardErrors != 3 {
		t.Fatalf("forwardErrors = %d, want 3 (cumulative count flows through)", s.ForwardErrors)
	}
	if s.LastForwardErr != errAt.UTC().Format(time.RFC3339) {
		t.Fatalf("lastForwardErr = %q, want the RFC3339 error time (feeds the UI decay)", s.LastForwardErr)
	}
	// forwardErrors must always serialize (no omitempty) so the card can distinguish
	// "0 errors" from a missing field.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"forwardErrors":3`) {
		t.Fatalf("expected forwardErrors in JSON, got %s", b)
	}
}

// With no forward errors the timestamp stays empty (omitempty drops it), so the UI
// never shows a decaying warning for a fault that never happened.
func TestBuildSnapshot_NoForwardErrLeavesTimestampEmpty(t *testing.T) {
	s := BuildSnapshot(fakeSource{driven: []string{"1"}})
	if s.LastForwardErr != "" {
		t.Fatalf("lastForwardErr = %q, want empty when no error occurred", s.LastForwardErr)
	}
}

// emptySource models a fresh, unpaired install: no provider, no TV clients.
type emptySource struct{}

func (emptySource) Version() string      { return "dev" }
func (emptySource) StartedAt() time.Time { return time.Time{} }
func (emptySource) ProInfo() (bool, string, string, string, bool) {
	return false, "", "", "", false
}
func (emptySource) TVClients() []string              { return nil }
func (emptySource) ModeInfo() (string, bool, bool)   { return "rest", false, false }
func (emptySource) BridgeName() string               { return "Philips Hue - ABCDEF" }
func (emptySource) LastActivity() time.Time          { return time.Time{} }
func (emptySource) LightsV1() (map[string]any, bool) { return nil, false }
func (emptySource) DrivenV1IDs() []string            { return nil }
func (emptySource) LiveColors() map[string]LiveColor { return nil }
func (emptySource) Active() bool                     { return false }
func (emptySource) StreamFPS() int                   { return 0 }
func (emptySource) ProSendFPS() int                  { return 0 }
func (emptySource) ProWriteRate() int                { return 0 }
func (emptySource) RestRecvRate() int                { return 0 }
func (emptySource) CoalesceRate() int                { return 0 }
func (emptySource) ForwardErrors() int               { return 0 }
func (emptySource) LastForwardErr() time.Time        { return time.Time{} }
func (emptySource) SmoothingTauMs() int              { return 40 }
func (emptySource) Jitter() (int, int, bool)         { return 0, 0, false }
func (emptySource) SetupComplete() bool              { return false }
func (emptySource) CurrentStep() int                 { return 1 }
func (emptySource) FirstRun() bool                   { return true }
func (emptySource) SetupInfo() (string, bool, bool, bool, bool, string) {
	return "", false, true, false, false, ""
}

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

// B: the TV activated a stream but DTLS to the Pro failed, so relume-tv reverted to
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
