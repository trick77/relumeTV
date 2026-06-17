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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"time"

	"github.com/trick77/relume/internal/config"
)

const appKeyHeader = "hue-application-key"

// putTrace gates temporary latency instrumentation for the REST control path.
// Enable by setting RELUME_PUT_TRACE=1; it logs per-PUT wall time and whether the
// underlying TCP/TLS connection was reused — to tell connection churn apart from
// genuine Bridge Pro round-trip latency. Remove once the Ambilight lag is diagnosed.
var putTrace = os.Getenv("RELUME_PUT_TRACE") != ""

// Client talks to a Hue Bridge Pro.
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
// optionally without TLS verification. The Bridge Pro uses a Signify CA certificate;
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

// Pair pairs with the Bridge Pro: the link button must be pressed. Returns
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
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
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
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	var out struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
		Data []struct {
			RID string `json:"rid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("POST %s: decode response: %w", path, err)
	}
	if len(out.Errors) > 0 {
		return "", fmt.Errorf("POST %s: %s", path, out.Errors[0].Description)
	}
	if len(out.Data) == 0 {
		return "", fmt.Errorf("POST %s: no resource id returned", path)
	}
	return out.Data[0].RID, nil
}

// put performs an authenticated CLIP v2 PUT (for REST control). The
// Bridge Pro responds with 200 (ok) or 207 (multi-status) and carries domain
// errors in the "errors" array; HTTP status errors (>=400) remain hard errors.
func (c *Client) put(path string, payload any) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, "https://"+c.host+path, bytes.NewReader(body))
	req.Header.Set(appKeyHeader, c.appKey)
	req.Header.Set("Content-Type", "application/json")

	var (
		traceStart time.Time
		reused     bool
		wasIdle    bool
		tlsStart   time.Time
		tlsDur     time.Duration
	)
	if putTrace {
		ct := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				reused, wasIdle = info.Reused, info.WasIdle
			},
			TLSHandshakeStart: func() { tlsStart = time.Now() },
			TLSHandshakeDone:  func(tls.ConnectionState, error) { tlsDur = time.Since(tlsStart) },
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), ct))
		traceStart = time.Now()
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if putTrace {
			slog.Info("bridgepro put trace", "path", path,
				"total_ms", time.Since(traceStart).Milliseconds(),
				"conn_reused", reused, "tls_handshake_ms", tlsDur.Milliseconds(), "err", err)
		}
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if putTrace {
		slog.Info("bridgepro put trace", "path", path,
			"total_ms", time.Since(traceStart).Milliseconds(),
			"conn_reused", reused, "conn_was_idle", wasIdle,
			"tls_handshake_ms", tlsDur.Milliseconds(), "status", resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	var out struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && len(out.Errors) > 0 {
		return fmt.Errorf("PUT %s: %s", path, out.Errors[0].Description)
	}
	return nil
}

// del performs an authenticated CLIP v2 DELETE. Used to remove relume's own
// entertainment_configuration when it no longer matches the current light set (so
// stale configs do not pile up on the Pro and hit its area limit).
func (c *Client) del(path string) error {
	req, _ := http.NewRequest(http.MethodDelete, "https://"+c.host+path, nil)
	req.Header.Set(appKeyHeader, c.appKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("DELETE %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	var out struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && len(out.Errors) > 0 {
		return fmt.Errorf("DELETE %s: %s", path, out.Errors[0].Description)
	}
	return nil
}
