// Package upnp renders the /description.xml that the TV fetches after SSDP
// discovery and checks for modelName/modelNumber in order to recognize the
// bridge as a Gen-2 Hue bridge.
package upnp

import (
	"strings"
	"text/template"

	"github.com/trick77/relume/internal/config"
)

type profileFields struct {
	Manufacturer    string
	ManufacturerURL string
}

// Options selects experimental compatibility tweaks for description.xml.
type Options struct {
	Profile            string
	DescriptionProfile string
	MediaServerAlias   bool
}

const (
	// ServerHeaderDefault is the exact SERVER header of a real Hue bridge
	// (verified via diyHue).
	ServerHeaderDefault   = "Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0"
	ServerHeaderAmbilight = "Linux/3.14.0 UPnP/1.0 IpBridge/1.67.0"
	ServerHeaderHass      = "Hue/1.0 UPnP/1.0 IpBridge/1.48.0"
)

// ServerHeader returns the UPnP HTTP/SSDP server signature for an identity
// profile.
func ServerHeader(profile string) string {
	switch profile {
	case "ambilight":
		return ServerHeaderAmbilight
	case "hass":
		return ServerHeaderHass
	default:
		return ServerHeaderDefault
	}
}

func fieldsForProfile(profile string) profileFields {
	if profile == "hass" {
		return profileFields{
			Manufacturer:    "Royal Philips Electronics",
			ManufacturerURL: "http://www.philips.com",
		}
	}
	return profileFields{
		Manufacturer:    "Signify",
		ManufacturerURL: "http://www.meethue.com",
	}
}

// modelName/modelNumber are exactly the values of a Philips Hue Bridge 2015 (BSB002);
// the TV only recognizes these as compatible.
const tmplText = `<?xml version="1.0" encoding="UTF-8" ?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion>
<major>1</major>
<minor>0</minor>
</specVersion>
<URLBase>http://{{.IP}}:{{.Port}}/</URLBase>
<device>
<deviceType>{{.DeviceType}}</deviceType>
<friendlyName>Relume ({{.IP}})</friendlyName>
<manufacturer>{{.Manufacturer}}</manufacturer>
<manufacturerURL>{{.ManufacturerURL}}</manufacturerURL>
<modelDescription>Philips hue Personal Wireless Lighting</modelDescription>
<modelName>Philips hue bridge 2015</modelName>
<modelNumber>BSB002</modelNumber>
<modelURL>http://www.meethue.com</modelURL>
<serialNumber>{{.Serial}}</serialNumber>
<UDN>uuid:{{.UUID}}</UDN>
<presentationURL>index.html</presentationURL>
</device>
</root>
`

var tmpl = template.Must(template.New("description").Parse(tmplText))

const ambilightReferenceTmplText = `<?xml version="1.0" encoding="UTF-8" ?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<URLBase>http://{{.IP}}:{{.Port}}/</URLBase>
<device>
<deviceType>{{.DeviceType}}</deviceType>
<friendlyName>Ambilight Bridge ({{.IP}})</friendlyName>
<manufacturer>{{.Manufacturer}}</manufacturer>
<manufacturerURL>{{.ManufacturerURL}}</manufacturerURL>
<modelDescription>Philips hue Personal Wireless Lighting</modelDescription>
<modelName>Philips hue bridge 2015</modelName>
<modelNumber>BSB002</modelNumber>
<modelURL>http://www.meethue.com</modelURL>
<serialNumber>{{.Serial}}</serialNumber>
<UDN>uuid:{{.UUID}}</UDN>
<presentationURL>index.html</presentationURL>
</device>
</root>
`

var ambilightReferenceTmpl = template.Must(template.New("ambilight-reference-description").Parse(ambilightReferenceTmplText))

// Render generates the description.xml for the given identity and address.
func Render(id config.Identity, ip string, port int) (string, error) {
	return RenderWithProfile(id, ip, port, "")
}

// RenderWithProfile generates description.xml for an identity profile. The empty
// profile keeps relume's default bridge identity; "ambilight" keeps the
// Signify/meethue fields used by Ambilight-specific Hue emulators; "hass"
// matches Home Assistant emulated-hue fields that public Philips TV reports
// have accepted.
func RenderWithProfile(id config.Identity, ip string, port int, profile string) (string, error) {
	return RenderWithOptions(id, ip, port, Options{Profile: profile})
}

// RenderWithOptions generates description.xml with optional compatibility tweaks.
func RenderWithOptions(id config.Identity, ip string, port int, opts Options) (string, error) {
	var sb strings.Builder
	fields := fieldsForProfile(opts.Profile)
	deviceType := "urn:schemas-upnp-org:device:Basic:1"
	if opts.MediaServerAlias {
		deviceType = "urn:schemas-upnp-org:device:MediaServer:1"
	}
	descriptionTemplate := tmpl
	if opts.DescriptionProfile == "ambilight-reference" {
		descriptionTemplate = ambilightReferenceTmpl
	}
	err := descriptionTemplate.Execute(&sb, struct {
		IP              string
		Port            int
		DeviceType      string
		Serial          string
		UUID            string
		Manufacturer    string
		ManufacturerURL string
	}{
		IP:              ip,
		Port:            port,
		DeviceType:      deviceType,
		Serial:          id.SerialForProfile(opts.Profile),
		UUID:            id.UUIDForProfile(opts.Profile),
		Manufacturer:    fields.Manufacturer,
		ManufacturerURL: fields.ManufacturerURL,
	})
	return sb.String(), err
}
