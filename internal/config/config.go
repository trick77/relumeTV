// Package config holds the persistent state of relume-tv: the stable
// fake bridge identity (towards the TV), the pairing tokens issued by the TV
// and the pairing data for the real Hue Bridge Pro.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Identity is the stable identity with which relume-tv presents itself to the TV
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
	// Name is the Hue Bridge Pro's user-set name, captured at pairing (best-effort).
	// Used purely as a human-friendly reference in logs.
	Name string `json:"name,omitempty"`
	// BridgeID is the Hue Bridge Pro's bridge id, captured at pairing (best-effort).
	// A stable reference in logs, independent of the IP.
	BridgeID string `json:"bridgeId,omitempty"`
	// DiscoveryID is the Pro's discovery id, captured at pairing, so a later
	// re-discovery can pick THIS bridge rather than bridges[0] on a multi-bridge LAN.
	DiscoveryID string `json:"discoveryId,omitempty"`
}

// LogValue renders the Hue Bridge Pro as inlined log attributes: its name and bridge
// id when known, always its host. Attach under an EMPTY key so the fields appear
// at top level without a redundant "pro." prefix (the upstream bridge is always a
// Pro), e.g. log.Warn("...", "", cfg.Pro). Renders pro=<none> when unpaired.
func (p *BridgePro) LogValue() slog.Value {
	if p == nil {
		return slog.GroupValue(slog.String("pro", "<none>"))
	}
	attrs := make([]slog.Attr, 0, 3)
	if p.Name != "" {
		attrs = append(attrs, slog.String("name", p.Name))
	}
	if p.BridgeID != "" {
		attrs = append(attrs, slog.String("id", p.BridgeID))
	}
	attrs = append(attrs, slog.String("host", p.Host))
	return slog.GroupValue(attrs...)
}

// CurrentSchemaVersion is the schema version this binary writes. Bump it whenever a
// breaking change to the on-disk layout needs an explicit migration. A loaded config
// with a higher version than this is refused (the binary is too old to understand it);
// a missing/zero version is treated as the first schema and stamped on the next save.
const CurrentSchemaVersion = 1

// Config is the entire persistent state.
type Config struct {
	// SchemaVersion is the on-disk layout version. See CurrentSchemaVersion.
	SchemaVersion int                 `json:"schemaVersion"`
	Identity      Identity            `json:"identity"`
	ApiUsers      map[string]*ApiUser `json:"apiUsers"`
	Pro           *BridgePro          `json:"bridgePro,omitempty"`
	// EntConfigID is the id of relume-tv's own entertainment_configuration on the Pro,
	// persisted so the entertainment streamer can reuse it across restarts instead of
	// re-finding (or recreating) it on each stream. Top-level rather than inside Pro
	// so SetPro's copy-on-write reconnect never clobbers it. Access via
	// LoadEntConfigID / SaveEntConfigID (mutex-guarded).
	EntConfigID string `json:"entConfigId,omitempty"`

	mu   sync.Mutex `json:"-"`
	path string     `json:"-"`
	// persist gates whether save() actually writes to disk. It is false during a
	// fresh setup (no config file existed at Load): the whole config — including the
	// generated identity, the Pro pairing and the TV credentials — stays in memory
	// until Commit() flips it true and writes the file once. The file's existence
	// therefore marks "setup complete": a restart mid-setup finds no file and starts
	// a fresh wizard. An existing file at Load sets persist=true, so runtime updates
	// (Pro reconnect, etc.) keep writing as before.
	persist bool
	// firstRun records that no config file existed at Load — i.e. this process began
	// a fresh setup. Drives the UI's "first run" label and the avahi-service hint.
	firstRun bool
}

// Load reads the config from path. If the file exists, the config is loaded and
// persistence is enabled (runtime updates write through). If it does NOT exist, a
// new config with a freshly generated identity is built IN MEMORY and NOT written:
// persistence stays off until Commit() is called at the end of a successful setup.
// This makes the file's existence mean "setup complete" — a restart mid-setup finds
// no file and reruns the wizard with a fresh identity (the TV must then re-pair).
func Load(path string) (*Config, error) {
	c := &Config{path: path, ApiUsers: map[string]*ApiUser{}}

	// Clean up an orphaned temp file from a previous crashed/failed save so it never
	// lingers as garbage next to the real config. Best-effort.
	_ = os.Remove(path + ".tmp")

	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		serial, gerr := generateSerial()
		if gerr != nil {
			return nil, gerr
		}
		c.SchemaVersion = CurrentSchemaVersion
		c.Identity = Identity{Serial: serial}
		// Deferred persistence: do NOT write the file here. It is created once by
		// Commit() when the setup completes (TV data flowing). persist stays false so
		// SetPro/AddApiUser/SaveEntConfigID only update memory until then.
		c.firstRun = true
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.SchemaVersion > CurrentSchemaVersion {
		return nil, fmt.Errorf("config schema version %d is newer than this build supports (%d); upgrade relume-tv", c.SchemaVersion, CurrentSchemaVersion)
	}
	// A zero version is a legacy/first-schema file: adopt the current version so the
	// next save stamps it. Add real per-version migrations here as the schema evolves.
	if c.SchemaVersion == 0 {
		c.SchemaVersion = CurrentSchemaVersion
	}
	if c.ApiUsers == nil {
		c.ApiUsers = map[string]*ApiUser{}
	}
	c.path = path
	// The file already existed: this is a committed install. Persist runtime updates.
	c.persist = true
	return c, nil
}

// Commit enables persistence and writes the config to disk exactly once, called when
// the setup completes (the first TV data flows). It is safe to call repeatedly: after
// the first commit persist is already true and saves behave normally. Callers must
// not hold any external lock that save() would need.
func (c *Config) Commit() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persist = true
	return c.save()
}

// Committed reports whether the config is being persisted to disk — true for an
// install loaded from an existing file, or after Commit() during a fresh setup.
// While false the setup is still in progress (nothing written yet).
func (c *Config) Committed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persist
}

// FirstRun reports whether no config file existed at Load (a fresh install). Used
// for the UI's "first run" label and the avahi-service completeness hint.
func (c *Config) FirstRun() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstRun
}

// AddApiUser creates a new paired TV client and persists it.
func (c *Config) AddApiUser(u *ApiUser) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ApiUsers[u.Username] = u
	return c.save()
}

// PairedDeviceTypes returns the devicetypes of all paired clients (no secrets),
// sorted — for a startup config summary.
func (c *Config) PairedDeviceTypes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.ApiUsers))
	for _, u := range c.ApiUsers {
		out = append(out, u.DeviceType)
	}
	sort.Strings(out)
	return out
}

// PSKForUser returns the DTLS pre-shared key (the hex-decoded clientkey) for a
// paired client identity (username), for the entertainment DTLS handshake.
func (c *Config) PSKForUser(username string) ([]byte, bool) {
	c.mu.Lock()
	u, ok := c.ApiUsers[username]
	c.mu.Unlock()
	if !ok || u.ClientKey == "" {
		return nil, false
	}
	key, err := hex.DecodeString(u.ClientKey)
	if err != nil {
		return nil, false
	}
	return key, true
}

// HasApiUser checks whether a username is known (paired).
func (c *Config) HasApiUser(username string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ApiUsers[username]
	return ok
}

// ApiUserByDeviceType returns the first paired user with the given devicetype, if
// any. Used to make pairing idempotent (the TV polls POST /api repeatedly).
func (c *Config) ApiUserByDeviceType(deviceType string) (*ApiUser, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range c.ApiUsers {
		if u.DeviceType == deviceType {
			return u, true
		}
	}
	return nil, false
}

// GetPro returns the current Hue Bridge Pro pairing data (nil if unpaired). Safe to
// call concurrently with SetPro, which autoPairPro/watchPro invoke from their own
// goroutines. SetPro replaces the pointer with a fresh *BridgePro rather than
// mutating fields in place, so the returned value stays immutable for the caller.
func (c *Config) GetPro() *BridgePro {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Pro
}

// SetPro stores the Hue Bridge Pro pairing data and persists it.
func (c *Config) SetPro(p *BridgePro) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Pro = p
	return c.save()
}

// LoadEntConfigID returns the persisted relume-tv entertainment_configuration id (empty
// if none). The streamer uses it as the first candidate to reuse across restarts.
func (c *Config) LoadEntConfigID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.EntConfigID
}

// SaveEntConfigID persists the relume-tv entertainment_configuration id (an empty id
// clears it, e.g. when the streamer deleted a stale config). A no-op write when the
// id is unchanged, so the streamer can call it freely without churning the file.
func (c *Config) SaveEntConfigID(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.EntConfigID == id {
		return nil
	}
	c.EntConfigID = id
	return c.save()
}

// save writes the config atomically and durably to c.path. The caller may hold the
// lock. The temp file is fsync'd before the rename and the directory is fsync'd after,
// so a crash/power loss leaves either the old or the new file intact, never a partial
// one. A failed rename removes the temp file so no garbage lingers.
func (c *Config) save() error {
	if c.path == "" {
		return nil
	}
	// Deferred persistence: during a fresh setup nothing is written to disk until
	// Commit() flips persist true. So SetPro/AddApiUser/SaveEntConfigID called mid-setup
	// only mutate the in-memory config; the file appears in one atomic write at Commit().
	if !c.persist {
		return nil
	}
	// SchemaVersion is always stamped by Load (both the fresh and legacy-zero paths),
	// and Load is the only constructor of *Config — so every config reaching save has
	// it set. No defensive re-stamp needed here.
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp) // don't leave a half-written temp behind
		return err
	}
	return fsyncDir(dir)
}

// writeFileSync writes data to path (0600) and fsyncs it before returning, so the
// bytes are on disk before the caller renames it into place.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(data); werr != nil {
		f.Close()
		return werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		return serr
	}
	return f.Close()
}

// fsyncDir flushes a directory entry (the rename) to disk. A failure to open/sync the
// directory is non-fatal on platforms that don't support it; the rename itself already
// succeeded, so a best-effort sync is enough.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil //nolint:nilerr // directory fsync is best-effort
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// generateSerial generates a random 12-digit hex serial (6 bytes).
func generateSerial() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate serial: %w", err)
	}
	return hex.EncodeToString(b), nil
}
