package bridgepro

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trick77/relume-tv/internal/config"
)

// hostOf strips the "https://" scheme from an httptest server URL, leaving
// "127.0.0.1:PORT" as client.go expects (it builds "https://"+host+path).
func hostOf(t *testing.T, serverURL string) string {
	t.Helper()
	return strings.TrimPrefix(serverURL, "https://")
}

// pinOf computes the SHA-256 hex fingerprint of the server's leaf certificate,
// matching what VerifyPeerCertificate hashes (rawCerts[0] == cert.Raw).
func pinOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test server has no certificate")
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// pinnedClient builds a Client pinned to the given fingerprint against srv.
func pinnedClient(t *testing.T, srv *httptest.Server, certSHA256 string) *Client {
	t.Helper()
	return New(&config.BridgePro{
		Host:          hostOf(t, srv.URL),
		AppKey:        "test-app-key",
		CertSHA256:    certSHA256,
		SkipTLSVerify: false,
	})
}

func TestSetLight_DomainError(t *testing.T) {
	// 207 multi-status with HTTP 200 but a non-empty errors[] body.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"description":"color.xy not supported"}],"data":[]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	err := c.SetLight("light-1", map[string]any{"color": map[string]any{"xy": map[string]any{"x": 0.5, "y": 0.5}}})
	if err == nil {
		t.Fatal("expected a domain error, got nil")
	}
	if !errors.Is(err, ErrDomain) {
		t.Fatalf("expected ErrDomain, got %v", err)
	}
	if !strings.Contains(err.Error(), "color.xy not supported") {
		t.Fatalf("expected description in message, got %v", err)
	}
}

func TestSetLight_DomainError_surfacesAllDescriptions(t *testing.T) {
	// A 207 multi-status can carry one error per attribute (e.g. several CT-only lights
	// each rejecting color.xy). Every description must reach the error, not just the first.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"description":"first failed"},{"description":"second failed"}],"data":[]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if err == nil || !errors.Is(err, ErrDomain) {
		t.Fatalf("expected ErrDomain, got %v", err)
	}
	if !strings.Contains(err.Error(), "first failed") || !strings.Contains(err.Error(), "second failed") {
		t.Fatalf("expected both descriptions in message, got %v", err)
	}
}

func TestSetLight_QueueFull(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`command queue is full`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
	if errors.Is(err, ErrUnreachable) || errors.Is(err, ErrDomain) {
		t.Fatalf("503 should be ErrQueueFull only, got %v", err)
	}
}

func TestSetLight_Unreachable(t *testing.T) {
	// Start a TLS server, grab its pin, then close it so the round-trip fails.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	pin := pinOf(t, srv)
	host := hostOf(t, srv.URL)
	srv.Close()

	c := New(&config.BridgePro{Host: host, AppKey: "k", CertSHA256: pin})
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
}

func TestSetLight_OK(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[],"data":[{"rid":"light-1","rtype":"light"}]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	if err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestCertPinning_Match(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	}))
	defer srv.Close()

	c := pinnedClient(t, srv, pinOf(t, srv))
	if err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}}); err != nil {
		t.Fatalf("matching pin should succeed, got %v", err)
	}
}

func TestCertPinning_Mismatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	}))
	defer srv.Close()

	// Wrong (but non-empty) fingerprint with SkipTLSVerify=false so
	// VerifyPeerCertificate is installed and the pin check actually runs.
	wrong := strings.Repeat("00", sha256.Size)
	c := pinnedClient(t, srv, wrong)
	err := c.SetLight("light-1", map[string]any{"on": map[string]any{"on": true}})
	if err == nil {
		t.Fatal("wrong pin should fail")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("pin mismatch should be ErrUnreachable (Do fails), got %v", err)
	}
}
