// Package bridgepro is the client for the real Hue Bridge Pro: pairing (app key),
// reading the CLIP v2 resources, control (REST fallback) and later activating
// the entertainment configuration. Communicates over HTTPS:443.
package bridgepro

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/trick77/relume-tv/internal/config"
)

const appKeyHeader = "hue-application-key"

// Error taxonomy for the Hue Bridge Pro client. Callers (e.g. watchPro) use
// errors.Is to distinguish error classes and react accordingly:
var (
	// ErrUnreachable means the HTTP round-trip itself failed: httpClient.Do
	// returned a non-nil error (connection refused, timeout, DNS, TLS handshake
	// or certificate-pin mismatch). The Pro should be re-discovered / re-pinned.
	ErrUnreachable = errors.New("bridge pro unreachable")
	// ErrQueueFull means the Pro answered HTTP 503: its command queue is full
	// under load. The Pro is reachable; the caller should back off, NOT
	// re-discover or re-pin.
	ErrQueueFull = errors.New("bridge pro command queue full")
	// ErrDomain means the HTTP response was OK (2xx, including 207 multi-status)
	// but the CLIP v2 body carried a non-empty errors[] array (e.g. a CT-only
	// light rejecting color.xy). This is a per-attribute domain rejection, not a
	// connectivity problem.
	ErrDomain = errors.New("bridge pro domain error")
)

// decodeCLIPErrors best-effort decodes the CLIP v2 {"errors":[{"description":...}]}
// shape from a response body and, if errors[] is non-empty, returns an
// ErrDomain-wrapped error including ALL descriptions joined. A 207 multi-status can
// carry one error per attribute (e.g. several CT-only lights each rejecting color.xy);
// surfacing only the first would hide the rest. A body that fails to unmarshal yields
// no domain error (nil) — the decode is deliberately lenient.
func decodeCLIPErrors(raw []byte) error {
	var out struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if len(out.Errors) > 0 {
		descs := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			descs = append(descs, e.Description)
		}
		return fmt.Errorf("%s: %w", strings.Join(descs, "; "), ErrDomain)
	}
	return nil
}

// Client talks to a Hue Bridge Pro.
// ProController is the Pro-facing read + control surface that the light provider
// and resilience code depend on, defined here (the producer package) so callers can
// program to the interface and inject fakes in tests without a live Hue Bridge Pro.
// *Client is the production implementation.
type ProController interface {
	// Lights returns the Pro's lights (CLIP v2, value types).
	Lights() ([]Light, error)
	// SetLight applies a CLIP v2 light state body to the light with the given UUID.
	SetLight(uuid string, v2body map[string]any) error
}

var _ ProController = (*Client)(nil)

type Client struct {
	host       string
	appKey     string
	httpClient *http.Client
}

// New creates a client from the persisted Pro pairing data.
func New(p *config.BridgePro) *Client {
	return &Client{
		host:       p.Host,
		appKey:     p.AppKey,
		httpClient: newHTTPClient(p.CertSHA256, p.SkipTLSVerify),
	}
}

// HTTPClientFor builds an HTTPS client for the given Pro data (for the
// setup flow before pairing, when no app key exists yet).
func HTTPClientFor(p *config.BridgePro) *http.Client {
	return newHTTPClient(p.CertSHA256, p.SkipTLSVerify)
}

// newHTTPClient builds an HTTPS client with certificate pinning (default) or
// optionally without TLS verification. The Hue Bridge Pro uses a Signify CA certificate;
// pinning to the leaf fingerprint avoids CA handling on the local network.
func newHTTPClient(certSHA256 string, skipVerify bool) *http.Client {
	tlsCfg := &tls.Config{
		// We verify ourselves (pinning) or deliberately skip — InsecureSkipVerify
		// only disables the standard chain; VerifyPeerCertificate handles the pinning.
		InsecureSkipVerify: true, //nolint:gosec // see VerifyPeerCertificate
	}
	if !skipVerify && certSHA256 != "" {
		want := certSHA256
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no certificate from the bridge")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("certificate fingerprint does not match (expected %s, got %s)", want, got)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
}

// FetchLeafFingerprint connects (without verification) and returns the SHA-256 hex
// of the leaf certificate — for the initial pinning in the setup flow.
func FetchLeafFingerprint(host string) (string, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 8 * time.Second},
		"tcp", host+":443",
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // we pin the fingerprint, not the chain
	)
	if err != nil {
		return "", fmt.Errorf("tls connection to %s: %w", host, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("no certificate received")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

// PairResult are the secrets obtained during pairing.
type PairResult struct {
	AppKey    string
	ClientKey string
}

// Pair pairs with the Hue Bridge Pro: the link button must be pressed. Returns
// the app key and clientkey (DTLS PSK). httpClient must already be configured
// correctly (pinning/skip-verify).
func Pair(httpClient *http.Client, host, deviceType string) (*PairResult, error) {
	body, _ := json.Marshal(map[string]any{
		"devicetype":        deviceType,
		"generateclientkey": true,
	})
	resp, err := httpClient.Post("https://"+host+"/api", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("pairing request: %w", err)
	}
	defer resp.Body.Close()

	var out []struct {
		Success *struct {
			Username  string `json:"username"`
			ClientKey string `json:"clientkey"`
		} `json:"success"`
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse pairing response: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty pairing response")
	}
	if out[0].Error != nil {
		return nil, fmt.Errorf("bridge: %s", out[0].Error.Description)
	}
	if out[0].Success == nil {
		return nil, fmt.Errorf("unexpected pairing response")
	}
	return &PairResult{AppKey: out[0].Success.Username, ClientKey: out[0].Success.ClientKey}, nil
}

// get performs an authenticated CLIP v2 GET and decodes the response.
func (c *Client) get(path string, v any) error {
	req, _ := http.NewRequest(http.MethodGet, "https://"+c.host+path, nil)
	req.Header.Set(appKeyHeader, c.appKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, errors.Join(err, ErrUnreachable))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: status %d: %s: %w", path, resp.StatusCode, string(b), ErrQueueFull)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// post performs an authenticated CLIP v2 POST and returns the rid of the created
// resource (CLIP v2 replies {"errors":[],"data":[{"rid":...,"rtype":...}]}). Used
// to create the entertainment_configuration in entertainment mode.
func (c *Client) post(path string, payload any) (string, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "https://"+c.host+path, bytes.NewReader(body))
	req.Header.Set(appKeyHeader, c.appKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, errors.Join(err, ErrUnreachable))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusServiceUnavailable {
		return "", fmt.Errorf("POST %s: status %d: %s: %w", path, resp.StatusCode, string(raw), ErrQueueFull)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	// Best-effort domain-error check (consistent with put/del).
	if derr := decodeCLIPErrors(raw); derr != nil {
		return "", fmt.Errorf("POST %s: %w", path, derr)
	}
	// post additionally requires the rid; this decode hard-errors.
	var out struct {
		Data []struct {
			RID string `json:"rid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("POST %s: decode response: %w", path, err)
	}
	if len(out.Data) == 0 {
		return "", fmt.Errorf("POST %s: no resource id returned", path)
	}
	return out.Data[0].RID, nil
}

// put performs an authenticated CLIP v2 PUT (for REST control). The
// Hue Bridge Pro responds with 200 (ok) or 207 (multi-status) and carries domain
// errors in the "errors" array; HTTP status errors (>=400) remain hard errors.
func (c *Client) put(path string, payload any) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, "https://"+c.host+path, bytes.NewReader(body))
	req.Header.Set(appKeyHeader, c.appKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, errors.Join(err, ErrUnreachable))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("PUT %s: status %d: %s: %w", path, resp.StatusCode, string(raw), ErrQueueFull)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	if derr := decodeCLIPErrors(raw); derr != nil {
		return fmt.Errorf("PUT %s: %w", path, derr)
	}
	return nil
}

// del performs an authenticated CLIP v2 DELETE. Used to remove relume-tv's own
// entertainment_configuration when it no longer matches the current light set (so
// stale configs do not pile up on the Pro and hit its area limit).
func (c *Client) del(path string) error {
	req, _ := http.NewRequest(http.MethodDelete, "https://"+c.host+path, nil)
	req.Header.Set(appKeyHeader, c.appKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", path, errors.Join(err, ErrUnreachable))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("DELETE %s: status %d: %s: %w", path, resp.StatusCode, string(raw), ErrQueueFull)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("DELETE %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	if derr := decodeCLIPErrors(raw); derr != nil {
		return fmt.Errorf("DELETE %s: %w", path, derr)
	}
	return nil
}
