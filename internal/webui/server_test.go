package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestServer_StateEndpoint(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var snap Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Version != "1.4.2" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestServer_StateHasNoSecrets(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	body := strings.ToLower(rec.Body.String())
	for _, banned := range []string{"appkey", "clientkey", "certsha", "psk", "username"} {
		if strings.Contains(body, banned) {
			t.Fatalf("state leaked %q: %s", banned, rec.Body.String())
		}
	}
}

func TestServer_FlashNilReturns404(t *testing.T) {
	srv := NewServer(":0", NewHub(8), fakeSource{}, nil, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestServer_FlashInvokesCallback(t *testing.T) {
	called := false
	srv := NewServer(":0", NewHub(8), fakeSource{}, func() error { called = true; return nil }, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil))
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("code=%d called=%v", rec.Code, called)
	}
}

func TestServer_FlashAllowsNoOrigin(t *testing.T) {
	// No Origin header (curl / direct LAN access — accepted by the threat model).
	called := false
	srv := NewServer(":0", NewHub(8), fakeSource{}, func() error { called = true; return nil }, discardLog())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil))
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("no-Origin POST should be allowed: code=%d called=%v", rec.Code, called)
	}
}

func TestServer_FlashAllowsSameOrigin(t *testing.T) {
	called := false
	srv := NewServer(":0", NewHub(8), fakeSource{}, func() error { called = true; return nil }, discardLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil)
	req.Host = "192.168.1.5:33300"
	req.Header.Set("Origin", "http://192.168.1.5:33300")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("same-origin POST should be allowed: code=%d called=%v", rec.Code, called)
	}
}

func TestServer_FlashRejectsCrossOrigin(t *testing.T) {
	called := false
	srv := NewServer(":0", NewHub(8), fakeSource{}, func() error { called = true; return nil }, discardLog())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/actions/flash", nil)
	req.Host = "192.168.1.5:33300"
	req.Header.Set("Origin", "http://evil.example.com")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("cross-origin POST should be rejected: code=%d called=%v", rec.Code, called)
	}
}

func TestServer_PeriodicSnapshotPublished(t *testing.T) {
	hub := NewHub(8)
	srv := NewServer(":0", hub, fakeSource{}, nil, discardLog())
	srv.snapInterval = 10 * time.Millisecond

	ch, cancel := hub.Subscribe()
	defer cancel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	go srv.runSnapshotLoop(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-ch:
			if f.Kind == "snapshot" && f.Snapshot != nil && f.Snapshot.Version == "1.4.2" {
				return // a snapshot was published over time, not just on connect
			}
		case <-deadline:
			t.Fatal("no periodic snapshot frame received — dashboard would not update live")
		}
	}
}

func TestServer_SSEStreamsInitialSnapshot(t *testing.T) {
	hub := NewHub(8)
	srv := httptest.NewServer(NewServer(":0", hub, fakeSource{}, nil, discardLog()).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), "\"kind\":\"snapshot\"") {
			return // success
		}
	}
	t.Fatal("no snapshot frame received")
}
