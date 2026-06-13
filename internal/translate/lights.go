// Package translate übersetzt zwischen dem CLIP-v2-Modell der Bridge Pro und der
// CLIP-v1-Darstellung, die der Ambilight-TV erwartet, inklusive eines stabilen
// Mappings zwischen v1-Light-IDs (numerisch) und v2-Ressourcen-UUIDs.
package translate

import (
	"strconv"

	"github.com/trick77/relume/internal/bridgepro"
)

// LightMap hält die v1-Darstellung der Lampen und das ID-Mapping für die Steuerung.
type LightMap struct {
	// V1 ist die CLIP-v1-Lampenliste (key = numerische ID als String).
	V1 map[string]any
	// V1ToUUID bildet die numerische v1-ID auf die v2-Ressourcen-UUID ab.
	V1ToUUID map[string]string
}

// LightsV1 übersetzt die v2-Lampen in die v1-Struktur. Die Bridge Pro liefert
// (CLIP v2) keine verlässlichen id_v1 mehr; daher vergeben wir stabile numerische
// IDs anhand der nach UUID sortierten Reihenfolge.
func LightsV1(lights []bridgepro.Light) LightMap {
	v1 := make(map[string]any, len(lights))
	rev := make(map[string]string, len(lights))
	for i, l := range lights {
		id := strconv.Itoa(i + 1)
		rev[id] = l.ID
		v1[id] = lightV1(l)
	}
	return LightMap{V1: v1, V1ToUUID: rev}
}

// lightV1 baut ein einzelnes v1-Light-Objekt aus einer v2-Lampe.
func lightV1(l bridgepro.Light) map[string]any {
	state := map[string]any{
		"on":        l.On.On,
		"bri":       briFromPercent(l.Dimming.Brightness),
		"alert":     "none",
		"reachable": true,
	}
	// Farb-/Weiss-Modus: v2 nutzt xy bzw. mirek.
	if l.Color.XY.X != 0 || l.Color.XY.Y != 0 {
		state["xy"] = []float64{l.Color.XY.X, l.Color.XY.Y}
		state["colormode"] = "xy"
	}
	if l.ColorTemperature.Mirek != 0 {
		state["ct"] = l.ColorTemperature.Mirek
		if _, hasXY := state["xy"]; !hasXY {
			state["colormode"] = "ct"
		}
	}
	name := l.Metadata.Name
	if name == "" {
		name = "Hue light " + l.ID[:8]
	}
	return map[string]any{
		"state":            state,
		"type":             "Extended color light",
		"name":             name,
		"modelid":          "LCT015",
		"manufacturername": "Signify Netherlands B.V.",
		"productname":      "Hue color lamp",
		"uniqueid":         l.ID,
		"swversion":        "1.122.2",
	}
}

// briFromPercent wandelt die v2-Helligkeit (0..100 %) in v1-bri (1..254).
func briFromPercent(pct float64) int {
	if pct <= 0 {
		return 1
	}
	bri := int(pct/100.0*253.0) + 1
	if bri > 254 {
		return 254
	}
	return bri
}
