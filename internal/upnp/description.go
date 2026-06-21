// Package upnp renders the /description.xml that the TV fetches after SSDP
// discovery and checks for modelName/modelNumber in order to recognize the
// bridge as a Gen-2 Hue bridge.
package upnp

import (
	"strings"
	"text/template"

	"github.com/trick77/relume-tv/internal/config"
)

// ServerHeaderDefault is the exact SERVER header of a real Hue bridge
// (verified via diyHue).
const ServerHeaderDefault = "Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0"

// modelName/modelNumber are exactly the values of a Philips Hue Bridge 2015 (BSB002);
// the TV only recognizes these as compatible. The manufacturer fields are the
// Signify/meethue values of a real Hue bridge.
const tmplText = `<?xml version="1.0" encoding="UTF-8" ?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion>
<major>1</major>
<minor>0</minor>
</specVersion>
<URLBase>http://{{.IP}}:{{.Port}}/</URLBase>
<device>
<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>
<friendlyName>relume-tv-{{.Suffix}}</friendlyName>
<manufacturer>Signify</manufacturer>
<manufacturerURL>http://www.meethue.com</manufacturerURL>
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

// Render generates the description.xml for the given identity and address.
func Render(id config.Identity, ip string, port int) (string, error) {
	// Suffix is the last 6 of the bridge id (the canonical Hue short identifier),
	// matching the TV-facing /config name so the bridge shows as "relume-tv-XXXXXX"
	// wherever the TV reads the name. Single token — the TV truncates at the first
	// space, so the previous "relume-tv (IP)" displayed as a bare "relume-tv".
	bridgeID := id.BridgeID()
	suffix := bridgeID
	if len(bridgeID) > 6 {
		suffix = bridgeID[len(bridgeID)-6:]
	}
	var sb strings.Builder
	err := tmpl.Execute(&sb, struct {
		IP     string
		Port   int
		Serial string
		UUID   string
		Suffix string
	}{
		IP:     ip,
		Port:   port,
		Serial: id.Serial,
		UUID:   id.UUID(),
		Suffix: suffix,
	})
	return sb.String(), err
}
