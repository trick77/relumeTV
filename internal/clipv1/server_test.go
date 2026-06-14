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
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestPairing_withoutLinkButton_thenFails(t *testing.T) {
	// Given
	_, ts := newTestServer(t)

	// When
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"tv"}`)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then
	if len(out) != 1 || out[0]["error"] == nil {
		t.Fatalf("expected error response, got %v", out)
	}
	if out[0]["error"]["type"].(float64) != 101 {
		t.Errorf("expected type 101, got %v", out[0]["error"]["type"])
	}
}

func TestPairing_autoPair_fromTVUserAgent_succeedsWithoutLinkButton(t *testing.T) {
	// Given: AutoPair on, no TV IP configured, no link button pressed
	s, ts := newTestServer(t)
	s.AutoPair = true

	// When: a request with the TV's Android/Dalvik User-Agent
	resp := mustPostUA(t, ts.URL+"/api", `{"devicetype":"65OLED806/12","generateclientkey":true}`, tvUserAgent)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then: paired without a button press
	if len(out) != 1 || out[0]["success"] == nil {
		t.Fatalf("expected success for TV auto-pair, got %v", out)
	}
	if username, _ := out[0]["success"]["username"].(string); len(username) != 32 {
		t.Errorf("username length = %d", len(username))
	}
}

func TestPairing_autoPair_fromNonTVRequest_stillFails(t *testing.T) {
	// Given: AutoPair on, no TV IP configured, no link button pressed
	s, ts := newTestServer(t)
	s.AutoPair = true

	// When: a request from a non-TV client (default Go User-Agent)
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"some-app"}`)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then: rejected with error 101 — auto-pair must not let arbitrary LAN devices pair
	if len(out) != 1 || out[0]["error"] == nil {
		t.Fatalf("expected error for non-TV auto-pair, got %v", out)
	}
	if out[0]["error"]["type"].(float64) != 101 {
		t.Errorf("expected type 101, got %v", out[0]["error"]["type"])
	}
}

func TestPairing_autoPair_fromConfiguredTVIP_succeeds(t *testing.T) {
	// Given: AutoPair on and the TV IP set to the loopback the test client uses
	s, ts := newTestServer(t)
	s.AutoPair = true
	s.TVIP = "127.0.0.1"

	// When: a non-TV User-Agent, but the source IP matches the configured TV
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"65OLED806/12","generateclientkey":true}`)
	defer resp.Body.Close()
	var out []map[string]map[string]any
	json.NewDecoder(resp.Body).Decode(&out)

	// Then: authorized by IP
	if len(out) != 1 || out[0]["success"] == nil {
		t.Fatalf("expected success for configured TV IP, got %v", out)
	}
}

func TestPairing_withLinkButton_thenReturnsUsernameAndClientKey(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.PressLink()

	// When
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`)
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
	s, ts := newTestServer(t)
	s.PressLink()
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`)
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

func TestGroupsExposeMinimalEntertainmentGroup(t *testing.T) {
	// Given
	s, ts := newTestServer(t)
	s.PressLink()
	resp := mustPost(t, ts.URL+"/api", `{"devicetype":"Philips_TV#Ambilight","generateclientkey":true}`)
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
