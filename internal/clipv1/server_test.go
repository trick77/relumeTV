package clipv1

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trick77/relume/internal/config"
	"github.com/trick77/relume/internal/upnp"
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
	s.pairAcceptDelay = 0 // tests pair immediately; the production delay is covered separately
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

func TestPairing_isDelayedThenAccepted(t *testing.T) {
	// Given: a short auto-pairing delay so the test stays fast
	s, ts := newTestServer(t)
	s.pairAcceptDelay = 120 * time.Millisecond
	body := `{"devicetype":"65OLED806/12","generateclientkey":true}`

	// When: the TV makes its first pairing attempt within the delay window
	r1 := mustPostUA(t, ts.URL+"/api", body, tvUserAgent)
	var o1 []map[string]map[string]any
	json.NewDecoder(r1.Body).Decode(&o1)
	r1.Body.Close()

	// Then: held off with the standard link-button error, like a real bridge
	if len(o1) != 1 || o1[0]["error"] == nil {
		t.Fatalf("expected 101 during the delay window, got %v", o1)
	}
	if o1[0]["error"]["type"].(float64) != 101 {
		t.Errorf("expected type 101, got %v", o1[0]["error"]["type"])
	}

	// When: the TV keeps polling past the delay
	time.Sleep(150 * time.Millisecond)
	r2 := mustPostUA(t, ts.URL+"/api", body, tvUserAgent)
	var o2 []map[string]map[string]any
	json.NewDecoder(r2.Body).Decode(&o2)
	r2.Body.Close()

	// Then: pairing is accepted
	if len(o2) != 1 || o2[0]["success"] == nil {
		t.Fatalf("expected success after the delay, got %v", o2)
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

func TestDescriptionXML_aliasVariantServesTextXMLContentType(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.MediaServerAlias = true

	// When
	resp := mustGet(t, ts.URL+"/description.xml?relume=ms1")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	// Then
	if got := resp.Header.Get("Content-Type"); got != "text/xml" {
		t.Errorf("Content-Type = %q, expected text/xml", got)
	}
}

func TestDescriptionXML_withHassProfileContainsHomeAssistantManufacturerFields(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "hass"

	// When
	resp := mustGet(t, ts.URL+"/description.xml")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	xml := string(body)
	for _, want := range []string{
		"<manufacturer>Royal Philips Electronics</manufacturer>",
		"<manufacturerURL>http://www.philips.com</manufacturerURL>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml does not contain %q:\n%s", want, xml)
		}
	}
}

func TestDescriptionXML_withAmbilightProfileKeepsSignifyManufacturerFields(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "ambilight"

	// When
	resp := mustGet(t, ts.URL+"/description.xml")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	if got := resp.Header.Get("Server"); got != upnp.ServerHeaderAmbilight {
		t.Errorf("Server header = %q, expected %q", got, upnp.ServerHeaderAmbilight)
	}
	if got := resp.Header.Get("Cache-Control"); got != "max-age=100" {
		t.Errorf("Cache-Control = %q, expected max-age=100", got)
	}
	xml := string(body)
	for _, want := range []string{
		"<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>",
		"<manufacturer>Signify</manufacturer>",
		"<manufacturerURL>http://www.meethue.com</manufacturerURL>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml does not contain %q:\n%s", want, xml)
		}
	}
}

func TestDescriptionXML_withAmbilightReferenceDescriptionProfile(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "ambilight"
	s.DescriptionProfile = "ambilight-reference"

	// When
	resp := mustGet(t, ts.URL+"/description.xml")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	xml := string(body)
	for _, want := range []string{
		"<specVersion><major>1</major><minor>0</minor></specVersion>",
		"<friendlyName>Ambilight Bridge (10.0.0.5)</friendlyName>",
		"<manufacturer>Signify</manufacturer>",
		"<modelNumber>BSB002</modelNumber>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml does not contain %q:\n%s", want, xml)
		}
	}
}

func TestDescriptionXML_withMediaServerAliasKeepsDefaultDeviceTypeForPlainPath(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "hass"
	s.MediaServerAlias = true

	// When
	resp := mustGet(t, ts.URL+"/description.xml")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	xml := string(body)
	for _, want := range []string{
		"<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>",
		"<manufacturer>Royal Philips Electronics</manufacturer>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml does not contain %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>") {
		t.Errorf("plain description.xml contains MediaServer deviceType:\n%s", xml)
	}
}

func TestDescriptionXML_withMediaServerAliasContainsMediaServerDeviceTypeForAliasQuery(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "hass"
	s.MediaServerAlias = true

	// When
	resp := mustGet(t, ts.URL+"/description.xml?relume=ms1")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	if got := resp.Header.Get("Cache-Control"); got != "max-age=1" {
		t.Errorf("Cache-Control = %q, expected max-age=1", got)
	}
	xml := string(body)
	for _, want := range []string{
		"<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>",
		"<manufacturer>Royal Philips Electronics</manufacturer>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml does not contain %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>") {
		t.Errorf("alias description.xml still contains Basic deviceType:\n%s", xml)
	}
}

func TestDescriptionXML_withMediaServerBasicBodyKeepsBasicDeviceTypeForAliasQuery(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "ambilight"
	s.MediaServerAlias = true
	s.MediaServerBasicBody = true

	// When
	resp := mustGet(t, ts.URL+"/description.xml?relume=ms1")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	if got := resp.Header.Get("Cache-Control"); got != "max-age=1" {
		t.Errorf("Cache-Control = %q, expected max-age=1", got)
	}
	xml := string(body)
	if !strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>") {
		t.Errorf("alias description.xml does not contain Basic deviceType:\n%s", xml)
	}
	if strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>") {
		t.Errorf("alias description.xml contains MediaServer deviceType:\n%s", xml)
	}
}

func TestDescriptionXML_withBasicDescriptorVariantKeepsBasicDeviceType(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "hass"
	s.MediaServerAlias = true

	// When
	resp := mustGet(t, ts.URL+"/description.xml?relume=basic1")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Then
	if got := resp.Header.Get("Cache-Control"); got != "max-age=1" {
		t.Errorf("Cache-Control = %q, expected max-age=1", got)
	}
	xml := string(body)
	if !strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>") {
		t.Errorf("basic descriptor variant does not contain Basic deviceType:\n%s", xml)
	}
	if strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>") {
		t.Errorf("basic descriptor variant contains MediaServer deviceType:\n%s", xml)
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
	if cfg["name"] != "Relume" {
		t.Errorf("name = %v, expected Relume", cfg["name"])
	}
}

func TestConfigWithAmbilightProfileReturnsShortConfigForUnknownUser(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.IdentityProfile = "ambilight"

	// When
	resp := mustGet(t, ts.URL+"/api/not-paired/config")
	defer resp.Body.Close()
	var cfg map[string]any

	// Then
	json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg["modelid"] != "BSB002" {
		t.Errorf("modelid = %v", cfg["modelid"])
	}
	if cfg["datastoreversion"] != "126" {
		t.Errorf("datastoreversion = %v", cfg["datastoreversion"])
	}
	if _, ok := cfg["error"]; ok {
		t.Fatalf("unexpected error response: %v", cfg)
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
