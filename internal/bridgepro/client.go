// Package bridgepro ist der Client zur echten Hue Bridge Pro: Kopplung (App-Key),
// Lesen der CLIP-v2-Ressourcen, Steuerung (REST-Fallback) und später das Aktivieren
// der Entertainment-Konfiguration. Kommuniziert über HTTPS:443.
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
	"net"
	"net/http"
	"time"

	"github.com/trick77/relume/internal/config"
)

const appKeyHeader = "hue-application-key"

// Client spricht mit einer Hue Bridge Pro.
type Client struct {
	host       string
	appKey     string
	httpClient *http.Client
}

// New erstellt einen Client aus den persistierten Pro-Kopplungsdaten.
func New(p *config.BridgePro) *Client {
	return &Client{
		host:       p.Host,
		appKey:     p.AppKey,
		httpClient: newHTTPClient(p.CertSHA256, p.SkipTLSVerify),
	}
}

// HTTPClientFor baut einen HTTPS-Client für die gegebenen Pro-Daten (für den
// setup-Flow vor dem Pairing, wenn noch kein App-Key vorliegt).
func HTTPClientFor(p *config.BridgePro) *http.Client {
	return newHTTPClient(p.CertSHA256, p.SkipTLSVerify)
}

// newHTTPClient baut einen HTTPS-Client mit Zertifikat-Pinning (Default) oder
// optional ohne TLS-Prüfung. Die Bridge Pro nutzt ein Signify-CA-Zertifikat;
// Pinning auf den Leaf-Fingerprint vermeidet das CA-Handling im lokalen Netz.
func newHTTPClient(certSHA256 string, skipVerify bool) *http.Client {
	tlsCfg := &tls.Config{
		// Wir prüfen selbst (Pinning) bzw. überspringen bewusst — InsecureSkipVerify
		// schaltet nur die Standard-Kette ab; VerifyPeerCertificate übernimmt das Pinning.
		InsecureSkipVerify: true, //nolint:gosec // siehe VerifyPeerCertificate
	}
	if !skipVerify && certSHA256 != "" {
		want := certSHA256
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("kein zertifikat von der bridge")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("zertifikat-fingerprint passt nicht (erwartet %s, bekam %s)", want, got)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
}

// FetchLeafFingerprint verbindet sich (ohne Prüfung) und liefert den SHA-256-Hex
// des Leaf-Zertifikats — für das initiale Pinning im setup-Flow.
func FetchLeafFingerprint(host string) (string, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 8 * time.Second},
		"tcp", host+":443",
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // wir pinnen den Fingerprint, keine Kette
	)
	if err != nil {
		return "", fmt.Errorf("tls-verbindung zu %s: %w", host, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("kein zertifikat erhalten")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

// PairResult sind die bei der Kopplung erhaltenen Geheimnisse.
type PairResult struct {
	AppKey    string
	ClientKey string
}

// Pair koppelt mit der Bridge Pro: der Link-Button muss gedrückt sein. Liefert
// App-Key und clientkey (DTLS-PSK). httpClient muss bereits korrekt konfiguriert
// sein (Pinning/skip-verify).
func Pair(httpClient *http.Client, host, deviceType string) (*PairResult, error) {
	body, _ := json.Marshal(map[string]any{
		"devicetype":        deviceType,
		"generateclientkey": true,
	})
	resp, err := httpClient.Post("https://"+host+"/api", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("pairing-request: %w", err)
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
		return nil, fmt.Errorf("pairing-antwort parsen: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("leere pairing-antwort")
	}
	if out[0].Error != nil {
		return nil, fmt.Errorf("bridge: %s", out[0].Error.Description)
	}
	if out[0].Success == nil {
		return nil, fmt.Errorf("unerwartete pairing-antwort")
	}
	return &PairResult{AppKey: out[0].Success.Username, ClientKey: out[0].Success.ClientKey}, nil
}

// get führt einen authentifizierten CLIP-v2-GET aus und decodiert die Antwort.
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

// put führt einen authentifizierten CLIP-v2-PUT aus (für REST-Steuerung). Die
// Bridge Pro antwortet mit 200 (ok) oder 207 (Multi-Status) und trägt fachliche
// Fehler im "errors"-Array; HTTP-Statusfehler (>=400) bleiben harte Fehler.
func (c *Client) put(path string, payload any) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, "https://"+c.host+path, bytes.NewReader(body))
	req.Header.Set(appKeyHeader, c.appKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

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
