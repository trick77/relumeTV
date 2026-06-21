package clipv1

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/relume-tv/internal/config"
)

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func mustPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func mustPostUA(t *testing.T, url, body, userAgent string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// tvUserAgent is the Android/Dalvik User-Agent a Philips Ambilight TV uses for
// CLIP v1 pairing.
const tvUserAgent = "Dalvik/2.1.0 (Linux; U; Android 11; 2021/22 Philips UHD Android TV Build/RTT2.211108.001)"

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "c.json"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s := New(cfg, "10.0.0.5", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestPairing_fromNonTVRequest_thenFails(t *testing.T) {
	// Given: auto-pair is the default, but only for the TV
	_, ts := newTestServer(t)

	// When: a non-TV client (default Go User-Agent) tries to pair
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"tv"}`)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then: rejected with error 101 — arbitrary LAN devices must not auto-pair
	if len(out) != 1 || out[0]["error"] == nil {
		t.Fatalf("expected error response, got %v", out)
	}
	if out[0]["error"]["type"].(float64) != 101 {
		t.Errorf("expected type 101, got %v", out[0]["error"]["type"])
	}
}

func TestPairing_fromTVUserAgent_thenReturnsUsernameAndClientKey(t *testing.T) {
	// Given: auto-pair default, request carries the TV's Android/Dalvik User-Agent
	_, ts := newTestServer(t)

	// When
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"65OLED806/12","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then
	if len(out) != 1 || out[0]["success"] == nil {
		t.Fatalf("expected success, got %v", out)
	}
	username, _ := out[0]["success"]["username"].(string)
	clientkey, _ := out[0]["success"]["clientkey"].(string)
	if len(username) != 32 {
		t.Errorf("username length = %d (%q)", len(username), username)
	}
	if len(clientkey) != 32 || clientkey != strings.ToUpper(clientkey) {
		t.Errorf("clientkey invalid: %q", clientkey)
	}

	// Then: paired user can fetch config
	cfgResp := mustGet(t, ts.URL+"/api/"+username+"/config")
	defer cfgResp.Body.Close()
	var cfg map[string]any
	json.NewDecoder(cfgResp.Body).Decode(&cfg)
	if cfg["modelid"] != "BSB002" {
		t.Errorf("modelid = %v, expected BSB002", cfg["modelid"])
	}
}

func TestPairing_isIdempotentForSameDeviceType(t *testing.T) {
	// Given: the TV polls POST /api rapidly with the same devicetype
	_, ts := newTestServer(t)
	body := `{"devicetype":"65OLED806/12","generateclientkey":true}`

	// When: two pairing requests for the same devicetype
	r1 := mustPostUA(t, ts.URL+"/api", body, tvUserAgent)
	var o1 []map[string]map[string]string
	json.NewDecoder(r1.Body).Decode(&o1)
	r1.Body.Close()
	r2 := mustPostUA(t, ts.URL+"/api", body, tvUserAgent)
	var o2 []map[string]map[string]string
	json.NewDecoder(r2.Body).Decode(&o2)
	r2.Body.Close()

	// Then: same credentials, no duplicate user minted
	u1 := o1[0]["success"]["username"]
	u2 := o2[0]["success"]["username"]
	if u1 == "" || u1 != u2 {
		t.Fatalf("expected identical username for same devicetype, got %q and %q", u1, u2)
	}
}

func TestPairing_acceptsFirstAttemptImmediately(t *testing.T) {
	// Given: a fresh server (no prior pairing) — there is no artificial accept delay
	_, ts := newTestServer(t)
	body := `{"devicetype":"65OLED806/12","generateclientkey":true}`

	// When: the TV makes its very first pairing attempt
	r1 := mustPostUA(t, ts.URL+"/api", body, tvUserAgent)
	var o1 []map[string]map[string]any
	json.NewDecoder(r1.Body).Decode(&o1)
	r1.Body.Close()

	// Then: it is accepted on the first try (no 101 retry loop / request spam)
	if len(o1) != 1 || o1[0]["success"] == nil {
		t.Fatalf("expected success on the first attempt, got %v", o1)
	}
	if o1[0]["error"] != nil {
		t.Errorf("expected no error on the first attempt, got %v", o1[0]["error"])
	}
	if u, _ := o1[0]["success"]["username"].(string); u == "" {
		t.Errorf("expected a non-empty username, got %v", o1[0]["success"])
	}
}

func TestPairing_fromConfiguredTVIP_succeeds(t *testing.T) {
	// Given: TV IP set to the loopback the test client connects from
	s, ts := newTestServer(t)
	s.TVIP = "127.0.0.1"

	// When: a non-TV User-Agent, but the source IP matches the configured TV
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"65OLED806/12"}`)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then: authorized by IP
	if len(out) != 1 || out[0]["success"] == nil {
		t.Fatalf("expected success for configured TV IP, got %v", out)
	}
}

func TestDescriptionXML_containsBSB002(t *testing.T) {
	// Given
	_, ts := newTestServer(t)

	// When
	resp := mustGet(t, ts.URL+"/description.xml")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	xml := string(body)
	for _, want := range []string{"<modelNumber>BSB002</modelNumber>", "Philips hue bridge 2015", "uuid:2f402f80-da50-11e1-9b23-"} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml does not contain %q:\n%s", want, xml)
		}
	}
}

func TestDescriptionXML_servesTextXMLContentType(t *testing.T) {
	// Real Hue bridges and the confirmed-working ha-hue-entertainment emulator
	// serve description.xml as text/xml; application/xml is suspected to make the
	// TV reject the descriptor and go silent before POST /api.

	// Given
	_, ts := newTestServer(t)

	// When
	resp := mustGet(t, ts.URL+"/description.xml")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	// Then
	if got := resp.Header.Get("Content-Type"); got != "text/xml" {
		t.Errorf("Content-Type = %q, expected text/xml", got)
	}
}

func TestShortConfig_unauthenticated(t *testing.T) {
	// Given
	_, ts := newTestServer(t)

	// When
	resp := mustGet(t, ts.URL+"/api/config")
	defer resp.Body.Close()
	var cfg map[string]any

	// Then
	json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg["modelid"] != "BSB002" {
		t.Errorf("modelid = %v", cfg["modelid"])
	}
	if cfg["factorynew"] != false {
		t.Errorf("factorynew = %v", cfg["factorynew"])
	}
	// The TV-visible name carries the bridge-id suffix so it is identifiable in the
	// Ambilight+Hue picker (not a bare "relume-tv").
	bridgeID, _ := cfg["bridgeid"].(string)
	wantName := "relume-tv-" + bridgeID[len(bridgeID)-6:]
	if cfg["name"] != wantName {
		t.Errorf("name = %v, expected %q", cfg["name"], wantName)
	}
}

func TestConfigDefaultProfileRejectsUnknownUser(t *testing.T) {
	// Given
	_, ts := newTestServer(t)

	// When
	resp := mustGet(t, ts.URL+"/api/not-paired/config")
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then
	if len(out) != 1 || out[0]["error"] == nil {
		t.Fatalf("expected error response, got %v", out)
	}
	if out[0]["error"]["type"].(float64) != 1 {
		t.Errorf("expected type 1, got %v", out[0]["error"]["type"])
	}
}

func TestCapabilitiesAndEmptyCollections(t *testing.T) {
	// Given
	_, ts := newTestServer(t)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When
	capResp := mustGet(t, ts.URL+"/api/"+username+"/capabilities")
	defer capResp.Body.Close()
	var caps map[string]any
	json.NewDecoder(capResp.Body).Decode(&caps)

	// Then
	streaming, ok := caps["streaming"].(map[string]any)
	if !ok {
		t.Fatalf("streaming capabilities missing: %v", caps)
	}
	if streaming["available"].(float64) != 1 || streaming["channels"].(float64) != 20 {
		t.Errorf("streaming capabilities = %v", streaming)
	}

	for _, path := range []string{"scenes", "schedules", "sensors", "rules", "resourcelinks"} {
		resp := mustGet(t, ts.URL+"/api/"+username+"/"+path)
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if len(out) != 0 {
			t.Errorf("%s = %v, expected empty object", path, out)
		}
	}
}

type fakeLightProvider struct{ lights map[string]any }

func (f fakeLightProvider) LightsV1() (map[string]any, error)       { return f.lights, nil }
func (f fakeLightProvider) SetLightV1(string, map[string]any) error { return nil }

// The TV reads the bulbs from the full datastore (GET /api/{user}), not from a
// separate /lights call — so the datastore MUST surface the paired Pro's lights,
// otherwise the TV reports "no hue color bulbs found".
func TestDatastore_includesLightsFromProvider(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.SetLightProvider(fakeLightProvider{lights: map[string]any{
		"1": map[string]any{"name": "Lamp", "type": "Extended color light"},
	}})
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When
	dsResp := mustGet(t, ts.URL+"/api/"+username)
	defer dsResp.Body.Close()
	var ds map[string]any
	json.NewDecoder(dsResp.Body).Decode(&ds)

	// Then
	lights, ok := ds["lights"].(map[string]any)
	if !ok || len(lights) != 1 {
		t.Fatalf("datastore lights = %v, want one light", ds["lights"])
	}
	if _, ok := lights["1"]; !ok {
		t.Fatalf("datastore lights missing id 1: %v", lights)
	}
}

// Without a paired Pro the datastore lights are an empty object, never missing.
func TestDatastore_lightsEmptyWhenNoProvider(t *testing.T) {
	// Given
	_, ts := newTestServer(t)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When
	dsResp := mustGet(t, ts.URL+"/api/"+username)
	defer dsResp.Body.Close()
	var ds map[string]any
	json.NewDecoder(dsResp.Body).Decode(&ds)

	// Then
	lights, ok := ds["lights"].(map[string]any)
	if !ok || len(lights) != 0 {
		t.Fatalf("datastore lights = %v, want empty object", ds["lights"])
	}
}

// LastActivity must advance when the TV writes a light state, so the idle-off
// monitor can detect the TV going silent. It must stay zero before any write.
func TestLastActivity_advancesOnLightStateWrite(t *testing.T) {
	// Given: a paired TV and a light provider
	s, ts := newTestServer(t)
	s.SetLightProvider(fakeLightProvider{lights: map[string]any{
		"1": map[string]any{"name": "Lamp", "type": "Extended color light"},
	}})
	if !s.LastActivity().IsZero() {
		t.Fatalf("LastActivity before any write = %v, want zero", s.LastActivity())
	}
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When: the TV writes a light state
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/"+username+"/lights/1/state", strings.NewReader(`{"on":true,"bri":254}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	put.Body.Close()

	// Then
	if s.LastActivity().IsZero() {
		t.Fatal("LastActivity still zero after a light-state write")
	}
}

// A dropped off-zone write is a true no-op: it must not advance LastActivity, or a TV
// writing only off-zone lights would keep the idle-off from ever firing.
func TestLastActivity_doesNotAdvanceOnOffZoneWrite(t *testing.T) {
	s, ts := newTestServer(t)
	s.SetLightProvider(&fanoutProvider{lights: map[string]any{"9": map[string]any{}}})
	user := pairTV(t, ts)

	// Given: the TV's Ambilight zone excludes light 9.
	s.setRequestedMembers([]uint16{3})

	// When: the TV writes per-light to the off-zone light 9.
	mustPut(t, ts.URL+"/api/"+user+"/lights/9/state", `{"on":true,"bri":254}`).Body.Close()

	// Then: the write was dropped, so it did not register as activity.
	if !s.LastActivity().IsZero() {
		t.Fatalf("LastActivity advanced on an off-zone write = %v, want zero", s.LastActivity())
	}
}

type recordingProvider struct {
	fakeLightProvider
	gotID    string
	gotState map[string]any
}

func (p *recordingProvider) SetLightV1(id string, state map[string]any) error {
	p.gotID, p.gotState = id, state
	return nil
}

func TestForwardLight_goesToProvider(t *testing.T) {
	// Given
	s, _ := newTestServer(t)
	rp := &recordingProvider{}
	s.SetLightProvider(rp)

	// When: the entertainment receiver forwards a decoded channel
	s.ForwardLight("6", map[string]any{"on": true, "bri": 200})

	// Then: it reaches the current provider's SetLightV1
	if rp.gotID != "6" || rp.gotState["bri"] != 200 {
		t.Fatalf("forwarded id=%q state=%v", rp.gotID, rp.gotState)
	}
}

func TestForwardLight_noProviderIsNoop(t *testing.T) {
	s, _ := newTestServer(t)
	s.ForwardLight("6", map[string]any{"on": true}) // must not panic with no provider
}

type fanoutProvider struct {
	lights map[string]any
	got    map[string]map[string]any
}

func (p *fanoutProvider) LightsV1() (map[string]any, error) { return p.lights, nil }
func (p *fanoutProvider) SetLightV1(id string, state map[string]any) error {
	if p.got == nil {
		p.got = map[string]map[string]any{}
	}
	p.got[id] = state
	return nil
}

func TestGroupAction_fansOutToAllLights(t *testing.T) {
	// Given: a paired TV and a provider exposing three lights
	s, ts := newTestServer(t)
	p := &fanoutProvider{lights: map[string]any{
		"1": map[string]any{}, "2": map[string]any{}, "3": map[string]any{},
	}}
	s.SetLightProvider(p)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	resp.Body.Close()
	username := paired[0]["success"]["username"]

	// When: the TV drives the group action path (instead of per-light PUTs)
	r := mustPut(t, ts.URL+"/api/"+username+"/groups/1/action", `{"on":true,"bri":200}`)
	r.Body.Close()

	// Then: the action is forwarded to every offered light, not silently dropped
	if len(p.got) != 3 {
		t.Fatalf("forwarded to %d lights, want 3: %v", len(p.got), p.got)
	}
	if p.got["2"]["bri"] != float64(200) {
		t.Errorf("light 2 state = %v, want bri 200", p.got["2"])
	}
}

func TestGroupAction_noLightsStillAcksOk(t *testing.T) {
	// Given: a paired TV but no light provider registered (no lights known)
	_, ts := newTestServer(t)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	resp.Body.Close()
	username := paired[0]["success"]["username"]

	// When: the TV drives a real group action with nothing to forward to
	r := mustPut(t, ts.URL+"/api/"+username+"/groups/1/action", `{"on":true,"bri":200}`)
	defer r.Body.Close()

	// Then: the handler still acks ok (the no-op is logged, see handleGroupAction)
	var ack []map[string]map[string]any
	json.NewDecoder(r.Body).Decode(&ack)
	if got := ack[0]["success"]["/groups/1/action"]; got != "ok" {
		t.Fatalf("group action ack = %v, want ok", ack)
	}
}

func TestMarkActivity_advancesLastActivity(t *testing.T) {
	// Given: a server with no activity yet (entertainment mode has no REST writes)
	s, _ := newTestServer(t)
	if !s.LastActivity().IsZero() {
		t.Fatalf("LastActivity before any activity = %v, want zero", s.LastActivity())
	}

	// When: a decoded entertainment frame marks activity
	s.MarkActivity()

	// Then: the idle-off monitor sees the TV as active
	if s.LastActivity().IsZero() {
		t.Fatal("LastActivity still zero after MarkActivity")
	}
}

func TestLightStateWriteID_matchesOnlyStatePUTs(t *testing.T) {
	cases := []struct {
		method, path string
		wantID       string
		wantOK       bool
	}{
		{"PUT", "/api/abc/lights/3/state", "3", true},
		{"PUT", "/api/abc/lights/12/state", "12", true},
		{"GET", "/api/abc/lights/3/state", "", false},       // not a write
		{"PUT", "/api/abc/lights/3", "", false},             // not a state path
		{"PUT", "/api/abc/groups/1/action", "", false},      // group, not light
		{"PUT", "/api/abc/lights//state", "", false},        // empty id
		{"PUT", "/api/abc/lights/3/state/extra", "", false}, // deeper path
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, "http://x"+c.path, nil)
		id, ok := lightStateWriteID(req)
		if ok != c.wantOK || id != c.wantID {
			t.Errorf("%s %s -> (%q,%v), want (%q,%v)", c.method, c.path, id, ok, c.wantID, c.wantOK)
		}
	}
}

func TestActivitySummary_accumulatesDistinctLightsAndResets(t *testing.T) {
	// Given: several light-state writes recorded, some repeating the same light
	s, _ := newTestServer(t)
	for _, id := range []string{"1", "1", "2", "3", "3", "3"} {
		s.recordLightWrite(id)
	}

	// When: the periodic summary fires
	var buf strings.Builder
	s.log = slog.New(slog.NewTextHandler(&buf, nil))
	s.flushActivity(30 * time.Second)

	// Then: it reports 6 writes across 3 distinct lights, then resets
	out := buf.String()
	if !strings.Contains(out, "light_state_writes=6") || !strings.Contains(out, "lights=3") {
		t.Fatalf("summary = %q", out)
	}
	buf.Reset()
	s.flushActivity(30 * time.Second) // nothing accumulated since reset
	if buf.Len() != 0 {
		t.Fatalf("expected no summary after reset, got %q", buf.String())
	}
}

func mustPut(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

func TestStreamActivation_entertainmentMode_confirmsAndReflectsOwner(t *testing.T) {
	// Given: entertainment mode is on and the TV is paired
	s, ts := newTestServer(t)
	s.EntertainmentMode = true
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When: the TV activates the entertainment stream
	actResp := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	defer actResp.Body.Close()
	var act []map[string]map[string]any
	json.NewDecoder(actResp.Body).Decode(&act)

	// Then: it is confirmed with the real v1 success shape (not a generic "ok")
	if got := act[0]["success"]["/groups/1/stream/active"]; got != true {
		t.Fatalf("activation response = %v", act)
	}

	// And: GET /groups/1 reflects the live stream with the owner set
	groupResp := mustGet(t, ts.URL+"/api/"+username+"/groups/1")
	defer groupResp.Body.Close()
	var group map[string]any
	json.NewDecoder(groupResp.Body).Decode(&group)
	stream := group["stream"].(map[string]any)
	if stream["active"] != true {
		t.Fatalf("stream not active: %v", stream)
	}
	if stream["owner"] != username {
		t.Fatalf("stream owner = %v, want %s", stream["owner"], username)
	}
}

func TestStreamActivation_restMode_keepsLegacyAck(t *testing.T) {
	// Given: REST mode (default, entertainment mode off)
	s, ts := newTestServer(t)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]
	_ = s

	// When: the TV activates the entertainment stream
	actResp := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	defer actResp.Body.Close()
	var act []map[string]map[string]any
	json.NewDecoder(actResp.Body).Decode(&act)

	// Then: the legacy generic ack is returned and the stream stays inactive
	if got := act[0]["success"]["/groups/1"]; got != "ok" {
		t.Fatalf("expected legacy ok ack, got %v", act)
	}
	groupResp := mustGet(t, ts.URL+"/api/"+username+"/groups/1")
	defer groupResp.Body.Close()
	var group map[string]any
	json.NewDecoder(groupResp.Body).Decode(&group)
	if group["stream"].(map[string]any)["active"] != false {
		t.Fatalf("stream should stay inactive without probe: %v", group["stream"])
	}
}

func TestStreamActivation_entertainmentMode_fallsBackToRESTOnDTLSTimeout(t *testing.T) {
	// Given: entertainment mode with a short DTLS watchdog and a paired TV
	s, ts := newTestServer(t)
	s.EntertainmentMode = true
	s.stream.fallbackTimeout = 30 * time.Millisecond
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When: the TV activates the stream — relume-tv confirms it for real (arms the watchdog)
	actResp := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	var act []map[string]map[string]any
	json.NewDecoder(actResp.Body).Decode(&act)
	actResp.Body.Close()
	if got := act[0]["success"]["/groups/1/stream/active"]; got != true {
		t.Fatalf("first activation should be confirmed, got %v", act)
	}

	// And: the TV never opens the DTLS stream → the watchdog fires
	time.Sleep(120 * time.Millisecond)

	// Then: relume-tv has fallen back to REST — a further activation gets the generic ack
	actResp2 := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	var act2 []map[string]map[string]any
	json.NewDecoder(actResp2.Body).Decode(&act2)
	actResp2.Body.Close()
	if got := act2[0]["success"]["/groups/1"]; got != "ok" {
		t.Fatalf("after fallback the activation should get the legacy REST ack, got %v", act2)
	}

	// And: GET /groups/1 reflects the stream as inactive
	groupResp := mustGet(t, ts.URL+"/api/"+username+"/groups/1")
	defer groupResp.Body.Close()
	var group map[string]any
	json.NewDecoder(groupResp.Body).Decode(&group)
	if group["stream"].(map[string]any)["active"] != false {
		t.Fatalf("stream should be inactive after fallback: %v", group["stream"])
	}
}

// End-to-end via handleGroupUpdate: once a fallback has latched, an activation
// before the recovery cooldown still gets the legacy REST ack, but an activation
// after the cooldown recovers entertainment and is confirmed for real. This locks
// in that the recovery check runs BEFORE confirmsEntertainment() — a reorder would
// make recovery silently never fire while the isolated unit test stays green.
func TestStreamActivation_entertainmentMode_recoversFromFallbackAfterCooldown(t *testing.T) {
	// Given: entertainment mode, a paired TV, a controllable clock and a latched fallback
	s, ts := newTestServer(t)
	s.EntertainmentMode = true
	clk := time.Unix(1_000, 0)
	s.stream.now = func() time.Time { return clk }
	s.SetDTLSFallbackRecovery(90 * time.Second)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	resp.Body.Close()
	username := paired[0]["success"]["username"]

	s.stream.watchdogFired(s.stream.gen, s.log) // latch fallback, stamping fallbackAt at the test clock
	if !s.stream.inFallback() {
		t.Fatal("precondition: should be in fallback")
	}

	// When: the TV re-activates BEFORE the cooldown elapses → still the legacy REST ack
	before := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	var b []map[string]map[string]any
	json.NewDecoder(before.Body).Decode(&b)
	before.Body.Close()
	if got := b[0]["success"]["/groups/1"]; got != "ok" {
		t.Fatalf("before cooldown: activation must still get the legacy REST ack, got %v", b)
	}

	// When: the cooldown has elapsed and the TV re-activates → recovered + confirmed for real
	clk = clk.Add(91 * time.Second)
	after := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	var a []map[string]map[string]any
	json.NewDecoder(after.Body).Decode(&a)
	after.Body.Close()
	if got := a[0]["success"]["/groups/1/stream/active"]; got != true {
		t.Fatalf("after cooldown: activation must be confirmed for real (recovered), got %v", a)
	}
	if s.stream.inFallback() {
		t.Fatal("fallback must be cleared after recovery")
	}
}

func TestStreamActivation_entertainmentMode_dtlsUpCancelsWatchdog(t *testing.T) {
	// Given: entertainment mode with a watchdog and a paired TV
	s, ts := newTestServer(t)
	s.EntertainmentMode = true
	s.stream.fallbackTimeout = 80 * time.Millisecond
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When: the TV activates the stream, opens its DTLS stream before the timeout, then
	// re-activates while the stream is healthy (must not arm a watchdog that falsely fires)
	actResp := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	actResp.Body.Close()
	s.MarkDTLSStreamUp()
	reAct := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	reAct.Body.Close()
	time.Sleep(140 * time.Millisecond)

	// Then: no fallback — a further activation is still confirmed for real
	actResp2 := mustPut(t, ts.URL+"/api/"+username+"/groups/1", `{"stream":{"active":true}}`)
	var act2 []map[string]map[string]any
	json.NewDecoder(actResp2.Body).Decode(&act2)
	actResp2.Body.Close()
	if got := act2[0]["success"]["/groups/1/stream/active"]; got != true {
		t.Fatalf("DTLS up → activation must stay confirmed (no false fallback), got %v", act2)
	}
}

// M1: a watchdog callback that began firing just before the stream came up must
// NOT stickily fall a HEALTHY TV back to REST. With the shared streamState lock and
// the generation token, markStreamUp deterministically wins the race. The loop +
// near-zero timeout maximizes the interleaving the -race detector can observe.
func TestStreamState_markStreamUpWinsRacingWatchdog(t *testing.T) {
	for i := 0; i < 200; i++ {
		ss := newStreamState(time.Nanosecond) // fire essentially immediately
		log := slog.New(slog.NewTextHandler(io.Discard, nil))

		// When: a watchdog is armed (and likely already firing) while the stream
		// comes up concurrently.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); ss.armWatchdog(true, log) }()
		go func() { defer wg.Done(); ss.markStreamUp(log) }()
		wg.Wait()

		// Give any in-flight watchdog callback a chance to run and (wrongly) flip
		// fallback before we assert.
		time.Sleep(time.Millisecond)

		// Then: the stream is up, so fallback must be clear.
		if ss.inFallback() {
			t.Fatalf("iteration %d: stream up but stuck in fallback", i)
		}
	}
}

// M1: a watchdog callback carrying a stale generation token (one that fired just
// before a disarm/re-arm superseded it) must no-op, even though the stream never
// came up (streamUp stays false). This isolates the generation-token guard — the
// streamUp check cannot catch this path — matching the spec's "stopped/superseded"
// invariant. In production the timer invokes watchdogFired with its captured gen,
// so calling it directly with a stale gen is an exact simulation.
func TestStreamState_supersededWatchdogNoOps(t *testing.T) {
	ss := newStreamState(time.Minute) // long timeout: the real timer won't fire during the test
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ss.armWatchdog(true, log) // gen -> 1
	ss.disarmWatchdog()       // gen -> 2, supersedes the gen=1 callback

	// When: the stale (gen=1) callback fires after being superseded — streamUp is
	// false, so ONLY the generation guard can stop the fallback.
	ss.watchdogFired(1, log)
	if ss.inFallback() {
		t.Fatal("a superseded (stale-generation) watchdog must not fall back")
	}

	// Control: a current-generation callback DOES fall back, proving the guard keys
	// on the gen value rather than blanket no-op'ing.
	ss.watchdogFired(ss.gen, log)
	if !ss.inFallback() {
		t.Fatal("current-generation watchdog should fall back")
	}
}

// M1 safety net: a genuine activation with NO stream-up must still flip to fallback
// so entertainment mode never leaves a non-streaming TV unfollowed.
func TestStreamState_genuineTimeoutFallsBack(t *testing.T) {
	ss := newStreamState(20 * time.Millisecond)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// When: armed but the TV never opens the DTLS stream.
	ss.armWatchdog(true, log)
	time.Sleep(80 * time.Millisecond)

	// Then: the real safety net fired.
	if !ss.inFallback() {
		t.Fatal("genuine timeout with no stream-up should fall back to REST")
	}
}

// Lazy recovery: a latched REST fallback clears on the next activation attempt once
// recoveryCooldown has elapsed (so a transient DTLS failure no longer pins the TV to
// REST until restart), but NOT before. The injectable clock avoids sleeping.
func TestStreamState_fallbackRecoversAfterCooldown(t *testing.T) {
	ss := newStreamState(time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := time.Unix(1_000, 0)
	ss.now = func() time.Time { return clk }
	ss.setRecoveryCooldown(90 * time.Second)

	// Given: a genuine timeout latched the fallback (fired directly with the initial
	// generation, so no real timer lingers).
	ss.watchdogFired(ss.gen, log)
	if !ss.inFallback() {
		t.Fatal("precondition: should be in fallback after a genuine timeout")
	}

	// When: an activation arrives before the cooldown elapses → no recovery.
	clk = clk.Add(30 * time.Second)
	if ss.tryRecoverFallback() {
		t.Fatal("must not recover before the cooldown elapses")
	}
	if !ss.inFallback() {
		t.Fatal("fallback must still be latched before the cooldown")
	}

	// When: an activation arrives after the cooldown → recovery.
	clk = clk.Add(61 * time.Second) // total 91s ≥ 90s cooldown
	if !ss.tryRecoverFallback() {
		t.Fatal("must recover once the cooldown has elapsed")
	}
	if ss.inFallback() {
		t.Fatal("fallback must be cleared after recovery")
	}
}

// Recovery disabled (recoveryCooldown ≤ 0) keeps the fallback sticky regardless of
// how long it has been latched.
func TestStreamState_fallbackStickyWhenRecoveryDisabled(t *testing.T) {
	ss := newStreamState(time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := time.Unix(1_000, 0)
	ss.now = func() time.Time { return clk }
	ss.setRecoveryCooldown(0) // disabled

	ss.watchdogFired(ss.gen, log)
	if !ss.inFallback() {
		t.Fatal("precondition: should be in fallback")
	}
	clk = clk.Add(time.Hour)
	if ss.tryRecoverFallback() {
		t.Fatal("recovery disabled: must never recover")
	}
	if !ss.inFallback() {
		t.Fatal("fallback must stay sticky when recovery is disabled")
	}
}

// M1 end-to-end via confirmsEntertainment(): after a racing stream-up, relume-tv must
// still confirm entertainment (not be stuck in REST fallback).
func TestServer_confirmsEntertainment_afterRacingStreamUp(t *testing.T) {
	for i := 0; i < 100; i++ {
		s, _ := newTestServer(t)
		s.EntertainmentMode = true
		s.stream.fallbackTimeout = time.Nanosecond

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.stream.armWatchdog(true, s.log) }()
		go func() { defer wg.Done(); s.MarkDTLSStreamUp() }()
		wg.Wait()
		time.Sleep(time.Millisecond)

		if !s.confirmsEntertainment() {
			t.Fatalf("iteration %d: stream up but confirmsEntertainment() is false (stuck in fallback)", i)
		}
	}
}

func TestStreamActiveFromBody(t *testing.T) {
	cases := []struct {
		body         string
		wantActive   bool
		wantHasField bool
	}{
		{`{"stream":{"active":true}}`, true, true},
		{`{"stream":{"active":false}}`, false, true},
		{`{"on":{"on":true}}`, false, false}, // ordinary group update, no stream field
		{`not json`, false, false},
	}
	for _, c := range cases {
		active, ok := streamActiveFromBody([]byte(c.body))
		if active != c.wantActive || ok != c.wantHasField {
			t.Fatalf("streamActiveFromBody(%q) = (%v,%v), want (%v,%v)", c.body, active, ok, c.wantActive, c.wantHasField)
		}
	}
}

// statsProvider is a light provider that also reports drain stats.
type statsProvider struct {
	fakeLightProvider
	coalesced, forwardErrors uint64
}

func (p statsProvider) DrainStatsDelta() (uint64, uint64) { return p.coalesced, p.forwardErrors }

func TestActivitySummary_includesActiveLightsAndForwardingStats(t *testing.T) {
	// Given: a server with a controlled-light set and a stats-reporting provider
	s, _ := newTestServer(t)
	s.ControlledLights = func() []string { return []string{"uuid-a", "uuid-b"} }
	s.SetLightProvider(statsProvider{coalesced: 7, forwardErrors: 2})
	s.recordLightWrite("1")
	s.recordWriteTime()

	// When: the summary fires
	var buf strings.Builder
	s.log = slog.New(slog.NewTextHandler(&buf, nil))
	s.flushActivity(30 * time.Second)

	// Then: it carries the active-light count + ids and the forwarding stats
	out := buf.String()
	for _, want := range []string{"active_lights=2", "active_light_ids=", "uuid-a", "coalesced_frames=7", "forward_errors=2", "since_last_write="} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q: %s", want, out)
		}
	}
}

func TestActivitySummary_includesGroupActionWritesAndHz(t *testing.T) {
	// Given: a mix of per-light and group-action writes over a 10s window
	s, _ := newTestServer(t)
	for _, id := range []string{"1", "2"} {
		s.recordLightWrite(id)
	}
	for i := 0; i < 8; i++ {
		s.recordGroupActionWrite()
	}

	// When: the summary fires
	var buf strings.Builder
	s.log = slog.New(slog.NewTextHandler(&buf, nil))
	s.flushActivity(10 * time.Second)

	// Then: it reports both paths and the derived total rate (10 writes / 10s = 1 Hz)
	out := buf.String()
	if !strings.Contains(out, "group_action_writes=8") || !strings.Contains(out, "total_hz=1") {
		t.Fatalf("summary = %q", out)
	}
}

func TestRequestLog_nonTVUserAgent_thenNotLoggedInNonDebug(t *testing.T) {
	// Given: a non-debug server with a captured log
	s, ts := newTestServer(t)
	var buf strings.Builder
	s.log = slog.New(slog.NewTextHandler(&buf, nil))

	// When: a non-TV LAN device (default Go User-Agent) probes a read path
	mustGet(t, ts.URL+"/api/abc/lights").Body.Close()

	// Then: the catch-all request log is suppressed — only TV traffic is logged
	if strings.Contains(buf.String(), "msg=http") {
		t.Fatalf("non-TV request should not be logged in non-debug mode: %s", buf.String())
	}
}

func TestRequestLog_tvUserAgent_thenLoggedInNonDebug(t *testing.T) {
	// Given: a non-debug server with a captured log
	s, ts := newTestServer(t)
	var buf strings.Builder
	s.log = slog.New(slog.NewTextHandler(&buf, nil))

	// When: the TV (Android/Dalvik User-Agent) issues the same read
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/abc/lights", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", tvUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	// Then: the request is logged
	if !strings.Contains(buf.String(), "msg=http") {
		t.Fatalf("TV request should be logged in non-debug mode: %s", buf.String())
	}
}

func TestRequestLog_tvLightPoll_thenAccumulatedNotLogged(t *testing.T) {
	// Given: a non-debug server with a captured log
	s, ts := newTestServer(t)
	var buf strings.Builder
	s.log = slog.New(slog.NewTextHandler(&buf, nil))

	// When: the TV polls a single light's state via GET /lights/{id}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/abc/lights/1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("User-Agent", tvUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	// Then: the per-request line is suppressed, but the poll is accumulated
	if strings.Contains(buf.String(), "msg=http") {
		t.Fatalf("light poll should not be logged per request: %s", buf.String())
	}

	// And: the activity rollup surfaces it as light_reads
	buf.Reset()
	s.flushActivity(time.Second)
	out := buf.String()
	if !strings.Contains(out, "ambilight activity") || !strings.Contains(out, "light_reads=1") {
		t.Fatalf("expected light_reads=1 in activity rollup, got %q", out)
	}
}

func TestGroupsExposeMinimalEntertainmentGroup(t *testing.T) {
	// Given
	_, ts := newTestServer(t)
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&paired)
	username := paired[0]["success"]["username"]

	// When
	groupsResp := mustGet(t, ts.URL+"/api/"+username+"/groups")
	defer groupsResp.Body.Close()
	var groups map[string]map[string]any
	json.NewDecoder(groupsResp.Body).Decode(&groups)

	groupResp := mustGet(t, ts.URL+"/api/"+username+"/groups/1")
	defer groupResp.Body.Close()
	var group map[string]any
	json.NewDecoder(groupResp.Body).Decode(&group)

	// Then
	if groups["1"]["type"] != "Entertainment" {
		t.Fatalf("groups[1] = %v", groups["1"])
	}
	if group["type"] != "Entertainment" {
		t.Fatalf("group 1 = %v", group)
	}
	stream, ok := group["stream"].(map[string]any)
	if !ok || stream["proxymode"] != "auto" {
		t.Fatalf("stream = %v", group["stream"])
	}
}

func TestParseGroupLights_realTVCreateBody(t *testing.T) {
	// The exact body the Ambilight TV sends (from the live log): a light subset plus
	// type/class. relume-tv must pull out the v1 ids and ignore the rest.
	body := []byte(`{"lights":["3","4"],"type":"Entertainment", "class":"TV"}`)
	ids, ok := parseGroupLights(body)
	if !ok {
		t.Fatalf("ok = false, want true for a body with a lights array")
	}
	if len(ids) != 2 || ids[0] != 3 || ids[1] != 4 {
		t.Fatalf("ids = %v, want [3 4]", ids)
	}
}

func TestParseGroupLights_noLightsArray_returnsNotOk(t *testing.T) {
	// A stream-activation PUT carries no lights — it must NOT be read as a subset
	// (which would clear an already-known one).
	for _, body := range []string{
		`{"stream":{"active":true}}`,
		`{"type":"Entertainment"}`,
		`{"lights":[]}`,
		`not json`,
	} {
		if ids, ok := parseGroupLights([]byte(body)); ok {
			t.Fatalf("body %q: ok=true ids=%v, want ok=false", body, ids)
		}
	}
}

func TestSetRequestedMembers_firesHookAndGatesAllowsMember(t *testing.T) {
	s, _ := newTestServer(t)
	var got []uint16
	s.OnGroupMembers = func(v1ids []uint16) { got = v1ids }

	// No subset yet → every light is allowed (defensive fallback).
	if !s.AllowsMember(99) {
		t.Fatalf("AllowsMember(99) = false before any subset, want true")
	}

	s.setRequestedMembers([]uint16{3, 4})

	if len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("OnGroupMembers got %v, want [3 4]", got)
	}
	if !s.AllowsMember(3) || !s.AllowsMember(4) {
		t.Fatalf("AllowsMember(3/4) = false, want true (in subset)")
	}
	if s.AllowsMember(5) {
		t.Fatalf("AllowsMember(5) = true, want false (outside subset)")
	}
}

// pairTV pairs the Ambilight TV and returns the issued username.
func pairTV(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var paired []map[string]map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&paired); err != nil {
		t.Fatalf("decode pairing: %v", err)
	}
	if len(paired) == 0 || paired[0]["success"] == nil {
		t.Fatalf("pairing failed: %v", paired)
	}
	return paired[0]["success"]["username"]
}

// A LightGroup POST (entertainment stickiness — the TV re-creates its group as a
// plain LightGroup) must NOT replace the Entertainment subset.
// This guards the type=="Entertainment" gate in handleCreateGroup.
func TestCreateGroup_lightGroupPOSTDoesNotClobberEntertainmentSubset(t *testing.T) {
	s, ts := newTestServer(t)
	user := pairTV(t, ts)

	// Given: the TV declared its Ambilight subset via an Entertainment group create.
	mustPost(t, ts.URL+"/api/"+user+"/groups", `{"lights":["3","4"],"type":"Entertainment","class":"TV"}`).Body.Close()
	if !s.AllowsMember(3) || s.AllowsMember(9) {
		t.Fatalf("after Entertainment create: AllowsMember(3)=%v AllowsMember(9)=%v, want true/false",
			s.AllowsMember(3), s.AllowsMember(9))
	}

	// When: a LightGroup naming a different light is posted.
	mustPost(t, ts.URL+"/api/"+user+"/groups", `{"lights":["9"],"type":"LightGroup"}`).Body.Close()

	// Then: the entertainment subset is untouched.
	if !s.AllowsMember(3) || !s.AllowsMember(4) || s.AllowsMember(9) {
		t.Fatalf("LightGroup create clobbered the subset: 3=%v 4=%v 9=%v, want true/true/false",
			s.AllowsMember(3), s.AllowsMember(4), s.AllowsMember(9))
	}
}

// A PUT to a group other than the entertainment group (id "1") must not change the
// subset. This guards the id=="1" gate in handleGroupUpdate.
func TestGroupUpdate_nonEntertainmentGroupDoesNotClobberSubset(t *testing.T) {
	s, ts := newTestServer(t)
	user := pairTV(t, ts)

	// Given: a known entertainment subset.
	mustPost(t, ts.URL+"/api/"+user+"/groups", `{"lights":["3","4"],"type":"Entertainment"}`).Body.Close()

	// When: another group is updated with a different light set.
	mustPut(t, ts.URL+"/api/"+user+"/groups/2", `{"lights":["9"]}`).Body.Close()

	// Then: the entertainment subset is untouched.
	if !s.AllowsMember(3) || !s.AllowsMember(4) || s.AllowsMember(9) {
		t.Fatalf("PUT /groups/2 clobbered the subset: 3=%v 4=%v 9=%v, want true/true/false",
			s.AllowsMember(3), s.AllowsMember(4), s.AllowsMember(9))
	}
}

// A group action must fan out only to the TV's requested Ambilight subset — lights
// in other rooms stay untouched. This guards the AllowsMember filter in
// handleGroupAction (counterpart to TestGroupAction_fansOutToAllLights).
func TestGroupAction_restrictedToRequestedSubset(t *testing.T) {
	s, ts := newTestServer(t)
	p := &fanoutProvider{lights: map[string]any{
		"1": map[string]any{}, "2": map[string]any{}, "3": map[string]any{},
	}}
	s.SetLightProvider(p)
	user := pairTV(t, ts)

	// Given: the TV restricts its Ambilight zone to light 3 only.
	s.setRequestedMembers([]uint16{3})

	// When: the TV drives the group action.
	mustPut(t, ts.URL+"/api/"+user+"/groups/1/action", `{"on":true,"bri":200}`).Body.Close()

	// Then: only light 3 is forwarded, not the off-zone lights 1 and 2.
	if len(p.got) != 1 {
		t.Fatalf("forwarded to %d lights, want 1 (only the subset): %v", len(p.got), p.got)
	}
	if _, ok := p.got["3"]; !ok {
		t.Fatalf("subset light 3 not forwarded: %v", p.got)
	}
}

// A per-light REST write to an off-zone light must be dropped: it must neither reach
// the Pro (so the light stays dark) nor taint the ControlledSet the restart/idle turn-off
// targets. The TV still gets a v1 success so it does not retry. Guards the AllowsMember
// gate in handleSetLightState (the per-light counterpart to the group-action gate).
func TestSetLightState_offZoneLightIsNotForwarded(t *testing.T) {
	s, ts := newTestServer(t)
	p := &fanoutProvider{lights: map[string]any{
		"3": map[string]any{}, "9": map[string]any{},
	}}
	s.SetLightProvider(p)
	user := pairTV(t, ts)

	// Given: the TV restricts its Ambilight zone to light 3 only.
	s.setRequestedMembers([]uint16{3})

	// When: the TV writes per-light to the in-zone light 3 and the off-zone light 9.
	mustPut(t, ts.URL+"/api/"+user+"/lights/3/state", `{"on":true,"bri":200}`).Body.Close()
	r := mustPut(t, ts.URL+"/api/"+user+"/lights/9/state", `{"on":true,"bri":200}`)
	defer r.Body.Close()

	// Then: only the in-zone light reaches the provider; the off-zone one is dropped.
	if _, ok := p.got["9"]; ok {
		t.Fatalf("off-zone light 9 was forwarded, want dropped: %v", p.got)
	}
	if _, ok := p.got["3"]; !ok {
		t.Fatalf("in-zone light 3 not forwarded: %v", p.got)
	}
	// And the off-zone write still acks success so the TV does not retry.
	var ack []map[string]map[string]any
	json.NewDecoder(r.Body).Decode(&ack)
	if len(ack) == 0 || ack[0]["success"] == nil {
		t.Fatalf("off-zone write did not ack success: %v", ack)
	}
}

// With no zone declared yet, a per-light write reaches every light (defensive
// fallback — nothing goes dark before the TV creates its Entertainment group).
func TestSetLightState_noSubsetForwardsAll(t *testing.T) {
	s, ts := newTestServer(t)
	p := &fanoutProvider{lights: map[string]any{"9": map[string]any{}}}
	s.SetLightProvider(p)
	user := pairTV(t, ts)

	mustPut(t, ts.URL+"/api/"+user+"/lights/9/state", `{"on":true,"bri":200}`).Body.Close()

	if _, ok := p.got["9"]; !ok {
		t.Fatalf("light 9 not forwarded with no subset declared: %v", p.got)
	}
}

func mustGetUA(t *testing.T, url, userAgent string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func TestDescriptor_OnDescriptorFetch_firesOnlyForTVRequest(t *testing.T) {
	// Given: a server with the descriptor hook wired
	s, ts := newTestServer(t)
	fired := 0
	s.OnDescriptorFetch = func() { fired++ }

	// When: a non-TV client (default Go UA) fetches the descriptor
	resp := mustGetUA(t, ts.URL+"/description.xml", "")
	resp.Body.Close()

	// Then: the hook does NOT fire for an arbitrary LAN probe
	if fired != 0 {
		t.Fatalf("OnDescriptorFetch fired for a non-TV request (count = %d)", fired)
	}

	// When: the TV (its Android/Dalvik UA) fetches the descriptor
	resp = mustGetUA(t, ts.URL+"/description.xml", tvUserAgent)
	resp.Body.Close()

	// Then: the hook fires exactly once
	if fired != 1 {
		t.Fatalf("OnDescriptorFetch fired %d times for a TV request, want 1", fired)
	}
}

func TestDescriptor_OnDescriptorFetch_nilSafe(t *testing.T) {
	// Given: no hook wired (nil)
	_, ts := newTestServer(t)

	// When/Then: a TV descriptor fetch must not panic
	resp := mustGetUA(t, ts.URL+"/description.xml", tvUserAgent)
	resp.Body.Close()
}
