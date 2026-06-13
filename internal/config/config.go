// Package config hält den persistenten Zustand von relume: die stabile
// Fake-Bridge-Identität (gegenüber dem TV), die vom TV vergebenen Pairing-Tokens
// und die Kopplungsdaten zur echten Hue Bridge Pro.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Identity ist die stabile Identität, mit der sich relume gegenüber dem TV
// als Gen-2-Bridge (BSB002) ausgibt. Sie wird einmalig erzeugt und persistiert,
// da der TV Bridge-Identitäten cached.
type Identity struct {
	// Serial ist die 12-stellige Hex-MAC ohne Trenner, lowercase (z.B. "2c4d54ea2832").
	// Entspricht serialNumber in description.xml und dem UUID-Suffix.
	Serial string `json:"serial"`
}

// MAC liefert die Serial im Doppelpunkt-Format (z.B. "2c:4d:54:ea:28:32").
func (i Identity) MAC() string {
	s := i.Serial
	var parts []string
	for j := 0; j+2 <= len(s); j += 2 {
		parts = append(parts, s[j:j+2])
	}
	return strings.Join(parts, ":")
}

// BridgeID ist die 16-stellige bridgeid (mac[:6] + FFFE + mac[6:], uppercase),
// wie sie der TV in /config und im hue-bridgeid SSDP-Header erwartet.
func (i Identity) BridgeID() string {
	s := i.Serial
	return strings.ToUpper(s[:6] + "fffe" + s[6:])
}

// UUID ist die UPnP-UUID; muss in SSDP-USN und description.xml UDN identisch sein.
func (i Identity) UUID() string {
	return "2f402f80-da50-11e1-9b23-" + i.Serial
}

// ApiUser ist ein vom TV gekoppelter Client.
type ApiUser struct {
	Username   string `json:"username"`
	DeviceType string `json:"deviceType"`
	// ClientKey ist der DTLS-PSK für den Entertainment-Pfad (nur bei generateclientkey).
	ClientKey string `json:"clientKey,omitempty"`
}

// BridgePro hält die Kopplungsdaten zur echten Hue Bridge Pro.
type BridgePro struct {
	Host string `json:"host"`
	// AppKey ist der Application-Key (CLIP-v2 hue-application-key).
	AppKey string `json:"appKey"`
	// ClientKey ist der DTLS-PSK für den Entertainment-Client zur Pro.
	ClientKey string `json:"clientKey"`
	// CertSHA256 ist der gepinnte SHA-256-Fingerprint des Pro-Leaf-Zertifikats (hex).
	CertSHA256 string `json:"certSha256,omitempty"`
	// SkipTLSVerify deaktiviert die TLS-Prüfung (Fallback statt Pinning).
	SkipTLSVerify bool `json:"skipTlsVerify"`
}

// Config ist der gesamte persistente Zustand.
type Config struct {
	Identity Identity            `json:"identity"`
	ApiUsers map[string]*ApiUser `json:"apiUsers"`
	Pro      *BridgePro          `json:"bridgePro,omitempty"`

	mu   sync.Mutex `json:"-"`
	path string     `json:"-"`
}

// Load liest die Config von path. Existiert sie nicht, wird eine neue mit frisch
// erzeugter Identität angelegt und sofort persistiert.
func Load(path string) (*Config, error) {
	c := &Config{path: path, ApiUsers: map[string]*ApiUser{}}

	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		serial, gerr := generateSerial()
		if gerr != nil {
			return nil, gerr
		}
		c.Identity = Identity{Serial: serial}
		if serr := c.save(); serr != nil {
			return nil, serr
		}
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("config lesen: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("config parsen: %w", err)
	}
	if c.ApiUsers == nil {
		c.ApiUsers = map[string]*ApiUser{}
	}
	c.path = path
	return c, nil
}

// AddApiUser legt einen neuen gekoppelten TV-Client an und persistiert.
func (c *Config) AddApiUser(u *ApiUser) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ApiUsers[u.Username] = u
	return c.save()
}

// HasApiUser prüft, ob ein Username bekannt (gekoppelt) ist.
func (c *Config) HasApiUser(username string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ApiUsers[username]
	return ok
}

// SetPro speichert die Bridge-Pro-Kopplungsdaten und persistiert.
func (c *Config) SetPro(p *BridgePro) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Pro = p
	return c.save()
}

// save schreibt die Config atomar nach c.path. Caller hält ggf. den Lock.
func (c *Config) save() error {
	if c.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config serialisieren: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// generateSerial erzeugt eine zufällige 12-stellige Hex-Serial (6 Bytes).
func generateSerial() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("serial erzeugen: %w", err)
	}
	return hex.EncodeToString(b), nil
}
