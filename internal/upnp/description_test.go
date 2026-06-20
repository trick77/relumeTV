package upnp

import (
	"strings"
	"testing"

	"github.com/trick77/relumetv/internal/config"
)

func TestRender_defaultFriendlyNameIsRelumeTV(t *testing.T) {
	// Given
	id := config.Identity{Serial: "2c4d54ea2832"}

	// When
	xml, err := Render(id, "192.0.2.10", 80)

	// Then: TV-visible name is relumeTV; discovery-critical fields unchanged
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		// Single-token name with the bridge-id suffix (BridgeID of this serial ends
		// EA2832). No space — the TV truncates the displayed name at the first space.
		"<friendlyName>relumeTV-EA2832</friendlyName>",
		"<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>",
		"<manufacturer>Signify</manufacturer>",
		"<manufacturerURL>http://www.meethue.com</manufacturerURL>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
		"<serialNumber>2c4d54ea2832</serialNumber>",
		"<UDN>uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832</UDN>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml missing %q:\n%s", want, xml)
		}
	}
}
