package upnp

import (
	"strings"
	"testing"

	"github.com/trick77/relume/internal/config"
)

func TestRenderWithProfile_hassUsesHomeAssistantManufacturerFields(t *testing.T) {
	// Given
	id := config.Identity{Serial: "2c4d54ea2832"}

	// When
	xml, err := RenderWithProfile(id, "192.0.2.10", 80, "hass")

	// Then
	if err != nil {
		t.Fatalf("RenderWithProfile: %v", err)
	}
	for _, want := range []string{
		"<manufacturer>Royal Philips Electronics</manufacturer>",
		"<manufacturerURL>http://www.philips.com</manufacturerURL>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml missing %q:\n%s", want, xml)
		}
	}
}

func TestServerHeader_usesProfileSpecificSignatures(t *testing.T) {
	tests := map[string]string{
		"":          ServerHeaderDefault,
		"ambilight": ServerHeaderAmbilight,
		"hass":      ServerHeaderHass,
		"unknown":   ServerHeaderDefault,
	}
	for profile, want := range tests {
		if got := ServerHeader(profile); got != want {
			t.Errorf("ServerHeader(%q) = %q, expected %q", profile, got, want)
		}
	}
}

func TestRenderWithProfile_ambilightUsesSignifyManufacturerFields(t *testing.T) {
	// Given
	id := config.Identity{Serial: "2c4d54ea2832"}

	// When
	xml, err := RenderWithProfile(id, "192.0.2.10", 80, "ambilight")

	// Then
	if err != nil {
		t.Fatalf("RenderWithProfile: %v", err)
	}
	for _, want := range []string{
		"<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>",
		"<manufacturer>Signify</manufacturer>",
		"<manufacturerURL>http://www.meethue.com</manufacturerURL>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
		"<serialNumber>2c4d54fffeea2832</serialNumber>",
		"<UDN>uuid:2f402f80-da50-11e1-9b23-2c4d54fffeea2832</UDN>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml missing %q:\n%s", want, xml)
		}
	}
}

func TestRenderWithOptions_mediaServerAliasUsesMediaServerDeviceType(t *testing.T) {
	// Given
	id := config.Identity{Serial: "2c4d54ea2832"}

	// When
	xml, err := RenderWithOptions(id, "192.0.2.10", 80, Options{
		Profile:          "hass",
		MediaServerAlias: true,
	})

	// Then
	if err != nil {
		t.Fatalf("RenderWithOptions: %v", err)
	}
	for _, want := range []string{
		"<deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>",
		"<manufacturer>Royal Philips Electronics</manufacturer>",
		"<modelName>Philips hue bridge 2015</modelName>",
		"<modelNumber>BSB002</modelNumber>",
		"<serialNumber>2c4d54ea2832</serialNumber>",
		"<UDN>uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832</UDN>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("description.xml missing %q:\n%s", want, xml)
		}
	}
	if strings.Contains(xml, "<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>") {
		t.Errorf("description.xml still contains Basic deviceType:\n%s", xml)
	}
}
