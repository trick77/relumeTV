// Package upnp rendert das /description.xml, das der TV nach der SSDP-Discovery
// abruft und auf modelName/modelNumber prüft, um die Bridge als Gen-2-Hue-Bridge
// zu erkennen.
package upnp

import (
	"strings"
	"text/template"

	"github.com/trick77/relume/internal/config"
)

// modelName/modelNumber sind exakt die Werte einer Philips Hue Bridge 2015 (BSB002);
// der TV erkennt nur diese als kompatibel.
const tmplText = `<?xml version="1.0" encoding="UTF-8" ?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion>
<major>1</major>
<minor>0</minor>
</specVersion>
<URLBase>http://{{.IP}}:{{.Port}}/</URLBase>
<device>
<deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>
<friendlyName>Philips hue ({{.IP}})</friendlyName>
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

// Render erzeugt das description.xml für die gegebene Identität und Adresse.
func Render(id config.Identity, ip string, port int) (string, error) {
	var sb strings.Builder
	err := tmpl.Execute(&sb, struct {
		IP     string
		Port   int
		Serial string
		UUID   string
	}{
		IP:     ip,
		Port:   port,
		Serial: id.Serial,
		UUID:   id.UUID(),
	})
	return sb.String(), err
}
