package bridgepro

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testClient wires a Client to a stub TLS server (same package → can set the
// unexported fields directly).
func testClient(srv *httptest.Server) *Client {
	return &Client{
		host:       strings.TrimPrefix(srv.URL, "https://"),
		appKey:     "test-app-key",
		httpClient: srv.Client(),
	}
}

func TestEntertainmentServices_mapsOwner(t *testing.T) {
	// Given
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clip/v2/resource/entertainment" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get(appKeyHeader) != "test-app-key" {
			t.Errorf("missing app key header")
		}
		_, _ = io.WriteString(w, `{"errors":[],"data":[
			{"id":"svc-1","owner":{"rid":"dev-1","rtype":"device"},"renderer":true}
		]}`)
	}))
	defer srv.Close()

	// When
	svcs, err := testClient(srv).EntertainmentServices()

	// Then
	if err != nil {
		t.Fatalf("EntertainmentServices: %v", err)
	}
	if len(svcs) != 1 || svcs[0].ID != "svc-1" || svcs[0].Owner.RID != "dev-1" {
		t.Fatalf("svcs = %+v", svcs)
	}
}

func TestCreateEntertainmentConfig_payloadAndID(t *testing.T) {
	// Given
	var gotBody map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/clip/v2/resource/entertainment_configuration" {
			t.Errorf("req = %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"errors":[],"data":[{"rid":"cfg-uuid","rtype":"entertainment_configuration"}]}`)
	}))
	defer srv.Close()

	// When
	id, err := testClient(srv).CreateEntertainmentConfig("relumetv", []ConfigMember{
		{ServiceRID: "svc-1"}, {ServiceRID: "svc-2", X: 0.5},
	})

	// Then
	if err != nil {
		t.Fatalf("CreateEntertainmentConfig: %v", err)
	}
	if id != "cfg-uuid" {
		t.Fatalf("id = %q", id)
	}
	if gotBody["configuration_type"] != "screen" {
		t.Errorf("configuration_type = %v", gotBody["configuration_type"])
	}
	if gotBody["metadata"].(map[string]any)["name"] != "relumetv" {
		t.Errorf("name = %v", gotBody["metadata"])
	}
	locs := gotBody["locations"].(map[string]any)["service_locations"].([]any)
	if len(locs) != 2 {
		t.Fatalf("service_locations = %d, want 2", len(locs))
	}
	svc := locs[0].(map[string]any)["service"].(map[string]any)
	if svc["rid"] != "svc-1" || svc["rtype"] != "entertainment" {
		t.Errorf("member service = %v", svc)
	}
}

func TestStartStopStream_bodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   func(c *Client) error
		action string
	}{
		{"start", func(c *Client) error { return c.StartStream("cfg-1") }, "start"},
		{"stop", func(c *Client) error { return c.StopStream("cfg-1") }, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given
			var gotBody map[string]any
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/clip/v2/resource/entertainment_configuration/cfg-1" {
					t.Errorf("req = %s %s", r.Method, r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotBody)
				_, _ = io.WriteString(w, `{"errors":[],"data":[]}`)
			}))
			defer srv.Close()

			// When / Then
			if err := tc.call(testClient(srv)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotBody["action"] != tc.action {
				t.Errorf("action = %v, want %s", gotBody["action"], tc.action)
			}
		})
	}
}

func TestDeleteEntertainmentConfig_method(t *testing.T) {
	// Given
	var gotMethod, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if r.Header.Get(appKeyHeader) != "test-app-key" {
			t.Errorf("missing app key header")
		}
		_, _ = io.WriteString(w, `{"errors":[],"data":[{"rid":"cfg-1","rtype":"entertainment_configuration"}]}`)
	}))
	defer srv.Close()

	// When
	err := testClient(srv).DeleteEntertainmentConfig("cfg-1")

	// Then
	if err != nil {
		t.Fatalf("DeleteEntertainmentConfig: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/clip/v2/resource/entertainment_configuration/cfg-1" {
		t.Fatalf("req = %s %s", gotMethod, gotPath)
	}
}

func TestGetEntertainmentConfig_channels(t *testing.T) {
	// Given
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clip/v2/resource/entertainment_configuration/cfg-1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"errors":[],"data":[{
			"id":"cfg-1","metadata":{"name":"relumetv"},"status":"inactive",
			"channels":[
				{"channel_id":0,"members":[{"service":{"rid":"svc-1","rtype":"entertainment"},"index":0}]},
				{"channel_id":1,"members":[{"service":{"rid":"svc-2","rtype":"entertainment"},"index":0}]}
			]}]}`)
	}))
	defer srv.Close()

	// When
	cfg, err := testClient(srv).GetEntertainmentConfig("cfg-1")

	// Then
	if err != nil {
		t.Fatalf("GetEntertainmentConfig: %v", err)
	}
	if len(cfg.Channels) != 2 || cfg.Channels[1].ChannelID != 1 {
		t.Fatalf("channels = %+v", cfg.Channels)
	}
	if cfg.Channels[0].Members[0].Service.RID != "svc-1" {
		t.Fatalf("member = %+v", cfg.Channels[0].Members)
	}
}
