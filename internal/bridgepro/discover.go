package bridgepro

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// ModelHueBridgePro is the modelid a real Hue Bridge Pro reports — in its mDNS TXT
// record (modelid=...) and in /api/0/config. relume-tv only drives a Pro, so a discovered
// bridge whose modelid differs is rejected as "not a Hue Bridge Pro". NOTE: this string
// is matched against the real hardware — if it is wrong, EVERY Pro is rejected, so any
// mismatch must surface the actual observed modelid loudly (see the selection callers).
const ModelHueBridgePro = "BSB003"

const (
	hueServiceName  = "_hue._tcp"
	mdnsDomain      = "local."
	discoverTimeout = 3 * time.Second
)

// DiscoveredBridge is a Hue bridge found on the local network via mDNS, with the
// bridgeid and modelid it advertises in its TXT record.
type DiscoveredBridge struct {
	ID                string
	InternalIPAddress string
	Port              int
	ModelID           string
}

// Discover browses the local network via mDNS (_hue._tcp.local.) for Hue bridges and
// returns each one's IPv4 plus the bridgeid and modelid from its TXT record. This is
// purely local — no Philips cloud, so no rate limits and no internet dependency. A
// powered-off bridge simply does not answer (empty result).
//
// excludeBridgeID drops relume-tv's OWN announcement: relume-tv advertises itself as a Hue
// bridge (_hue._tcp, modelid BSB002) to the TV, so without this filter a setup with no
// real bridge present would "discover" relume-tv and mislabel it as a non-Pro bridge.
func Discover(excludeBridgeID string) ([]DiscoveredBridge, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mdns resolver: %w", err)
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()
	if err := resolver.Browse(ctx, hueServiceName, mdnsDomain, entries); err != nil {
		return nil, fmt.Errorf("mdns browse: %w", err)
	}

	seen := map[string]bool{}
	var out []DiscoveredBridge
	// Browse closes entries when the context expires (after discoverTimeout), so this
	// range collects everything that answered within the window, then returns.
	for e := range entries {
		ip := firstIPv4(e.AddrIPv4)
		if ip == "" {
			continue
		}
		bridgeID, modelID := parseHueTXT(e.Text)
		if excludeBridgeID != "" && strings.EqualFold(bridgeID, excludeBridgeID) {
			continue // relume-tv's own announcement
		}
		key := bridgeID
		if key == "" {
			key = ip
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, DiscoveredBridge{ID: bridgeID, InternalIPAddress: ip, Port: e.Port, ModelID: modelID})
	}
	return out, nil
}

// parseHueTXT extracts the bridgeid and modelid from a Hue bridge's mDNS TXT record
// (entries like "bridgeid=001788FFFE..." and "modelid=BSB003"). Missing keys yield "".
func parseHueTXT(txt []string) (bridgeID, modelID string) {
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "bridgeid":
			bridgeID = strings.TrimSpace(v)
		case "modelid":
			modelID = strings.TrimSpace(v)
		}
	}
	return bridgeID, modelID
}

// firstIPv4 returns the first IPv4 address as a string (Hue bridges are IPv4-only), or
// "" if none is present.
func firstIPv4(ips []net.IP) string {
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// FetchModelID reads the unauthenticated short config (GET https://<host>/api/0/config)
// and returns the bridge's modelid. Used as a fallback to confirm the model when a
// discovered bridge did not advertise modelid in its mDNS TXT record. No app key is
// needed (this endpoint is open) and no certificate is pinned yet at discovery time, so
// TLS verification is skipped — the same posture as FetchLeafFingerprint.
func FetchModelID(host string) (string, error) {
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // pre-pairing discovery, no cert pinned yet
		},
	}
	resp, err := client.Get("https://" + host + "/api/0/config")
	if err != nil {
		return "", fmt.Errorf("fetch modelid from %s: %w", host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch modelid from %s: status %d", host, resp.StatusCode)
	}
	var cfg struct {
		ModelID string `json:"modelid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return "", fmt.Errorf("parse modelid from %s: %w", host, err)
	}
	return cfg.ModelID, nil
}
