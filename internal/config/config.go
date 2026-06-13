// Package config holds the persistent state of relume: the stable
// fake bridge identity (towards the TV), the pairing tokens issued by the TV
// and the pairing data for the real Hue Bridge Pro.
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

// Identity is the stable identity with which relume presents itself to the TV
// as a Gen 2 bridge (BSB002). It is generated once and persisted,
// because the TV caches bridge identities.
type Identity struct {
	// Serial is the 12-digit hex MAC without separators, lowercase (e.g. "2c4d54ea2832").
	// Corresponds to serialNumber in description.xml and the UUID suffix.
	Serial string `json:"serial"`
}

// MAC returns the serial in colon format (e.g. "2c:4d:54:ea:28:32").
func (i Identity) MAC() string {
	s := i.Serial
	var parts []string
	for j := 0; j+2 <= len(s); j += 2 {
		parts = append(parts, s[j:j+2])
	}
	return strings.Join(parts, ":")
}

// BridgeID is the 16-digit bridgeid (mac[:6] + FFFE + mac[6:], uppercase),
// as the TV expects it in /config and in the hue-bridgeid SSDP header.
func (i Identity) BridgeID() string {
	s := i.Serial
	return strings.ToUpper(s[:6] + "fffe" + s[6:])
}

// UUID is the UPnP UUID; must be identical in the SSDP USN and the description.xml UDN.
func (i Identity) UUID() string {
	return "2f402f80-da50-11e1-9b23-" + i.Serial
}

// SerialForProfile returns the serialNumber value advertised in description.xml.
func (i Identity) SerialForProfile(profile string) string {
	if profile == "ambilight" {
		return strings.ToLower(i.BridgeID())
	}
	return i.Serial
}

// UUIDForProfile returns the UPnP UUID used in SSDP USN and description.xml UDN.
func (i Identity) UUIDForProfile(profile string) string {
	return "2f402f80-da50-11e1-9b23-" + i.SerialForProfile(profile)
}

// ApiUser is a client paired by the TV.
type ApiUser struct {
	Username   string `json:"username"`
	DeviceType string `json:"deviceType"`
	// ClientKey is the DTLS PSK for the entertainment path (only with generateclientkey).
	ClientKey string `json:"clientKey,omitempty"`
}

// BridgePro holds the pairing data for the real Hue Bridge Pro.
type BridgePro struct {
	Host string `json:"host"`
	// AppKey is the application key (CLIP v2 hue-application-key).
	AppKey string `json:"appKey"`
	// ClientKey is the DTLS PSK for the entertainment client to the Pro.
	ClientKey string `json:"clientKey"`
	// CertSHA256 is the pinned SHA-256 fingerprint of the Pro leaf certificate (hex).
	CertSHA256 string `json:"certSha256,omitempty"`
	// SkipTLSVerify disables TLS verification (fallback instead of pinning).
	SkipTLSVerify bool `json:"skipTlsVerify"`
}

// Config is the entire persistent state.
type Config struct {
	Identity Identity            `json:"identity"`
	ApiUsers map[string]*ApiUser `json:"apiUsers"`
	Pro      *BridgePro          `json:"bridgePro,omitempty"`

	mu   sync.Mutex `json:"-"`
	path string     `json:"-"`
}

// Load reads the config from path. If it does not exist, a new one with a freshly
// generated identity is created and immediately persisted.
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
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.ApiUsers == nil {
		c.ApiUsers = map[string]*ApiUser{}
	}
	c.path = path
	return c, nil
}

// AddApiUser creates a new paired TV client and persists it.
func (c *Config) AddApiUser(u *ApiUser) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ApiUsers[u.Username] = u
	return c.save()
}

// HasApiUser checks whether a username is known (paired).
func (c *Config) HasApiUser(username string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ApiUsers[username]
	return ok
}

// SetPro stores the Bridge Pro pairing data and persists it.
func (c *Config) SetPro(p *BridgePro) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Pro = p
	return c.save()
}

// save writes the config atomically to c.path. The caller may hold the lock.
func (c *Config) save() error {
	if c.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
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

// generateSerial generates a random 12-digit hex serial (6 bytes).
func generateSerial() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate serial: %w", err)
	}
	return hex.EncodeToString(b), nil
}
