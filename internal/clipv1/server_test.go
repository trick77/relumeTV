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
		t.Fatalf("erwartete error-antwort, bekam %v", out)
	}
	if out[0]["error"]["type"].(float64) != 101 {
		t.Errorf("erwartete typ 101, bekam %v", out[0]["error"]["type"])
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
		t.Fatalf("erwartete success, bekam %v", out)
	}
	username, _ := out[0]["success"]["username"].(string)
	clientkey, _ := out[0]["success"]["clientkey"].(string)
	if len(username) != 32 {
		t.Errorf("username länge = %d (%q)", len(username), username)
	}
	if len(clientkey) != 32 || clientkey != strings.ToUpper(clientkey) {
		t.Errorf("clientkey ungültig: %q", clientkey)
	}

	// Then: gekoppelter user kann config abrufen
	cfgResp := mustGet(t, ts.URL+"/api/"+username+"/config")
	defer cfgResp.Body.Close()
	var cfg map[string]any
	json.NewDecoder(cfgResp.Body).Decode(&cfg)
	if cfg["modelid"] != "BSB002" {
		t.Errorf("modelid = %v, erwartet BSB002", cfg["modelid"])
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
			t.Errorf("description.xml enthält %q nicht:\n%s", want, xml)
		}
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
