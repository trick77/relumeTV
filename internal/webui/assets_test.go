package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexServesAppShell(t *testing.T) {
	srv := NewServer(":0", NewHub(1), fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{"app.js", "app.css", `id="app"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("index.html missing %q", marker)
		}
	}
}

func TestAppJSReferencesEndpoints(t *testing.T) {
	srv := NewServer(":0", NewHub(1), fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	body := rec.Body.String()
	for _, marker := range []string{"/api/state", "/api/events"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("app.js missing %q", marker)
		}
	}
}
