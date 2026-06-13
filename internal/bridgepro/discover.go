package bridgepro

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DiscoveredBridge ist ein Eintrag der Philips-Cloud-Discovery.
type DiscoveredBridge struct {
	ID                string `json:"id"`
	InternalIPAddress string `json:"internalipaddress"`
	Port              int    `json:"port"`
}

// Discover fragt die Philips-Cloud-Discovery (discovery.meethue.com) nach Bridges
// im selben öffentlichen Netz. Benötigt Internetzugang; liefert die lokalen IPs.
func Discover() ([]DiscoveredBridge, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("https://discovery.meethue.com/")
	if err != nil {
		return nil, fmt.Errorf("cloud-discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloud-discovery: status %d", resp.StatusCode)
	}
	var bridges []DiscoveredBridge
	if err := json.NewDecoder(resp.Body).Decode(&bridges); err != nil {
		return nil, fmt.Errorf("cloud-discovery parsen: %w", err)
	}
	return bridges, nil
}
