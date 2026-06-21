// Package translate translates between the CLIP v2 model of the Hue Bridge Pro and the
// CLIP v1 representation that the Ambilight TV expects, including a stable
// mapping between v1 light IDs (numeric) and v2 resource UUIDs.
package translate

import (
	"strconv"

	"github.com/trick77/relume-tv/internal/bridgepro"
)

// LightMap holds the v1 representation of the lights and the ID mapping for control.
type LightMap struct {
	// V1 is the CLIP v1 light list (key = numeric ID as string).
	V1 map[string]any
	// V1ToUUID maps the numeric v1 ID to the v2 resource UUID.
	V1ToUUID map[string]string
}

// LightsV1 translates the v2 lights into the v1 structure. Only color-capable
// lights are offered: the Ambilight stream pushes xy colors, so non-color bulbs
// (white/CT/dimmable/on-off) cannot follow it and would make the Hue Bridge Pro
// reject the xy writes (207). Numeric v1 IDs are assigned over the KEPT lights in
// the caller's (UUID-sorted) order, so they stay stable while the bulb set is.
func LightsV1(lights []bridgepro.Light) LightMap {
	v1 := make(map[string]any, len(lights))
	rev := make(map[string]string, len(lights))
	id := 0
	for _, l := range lights {
		if !l.HasColor() {
			continue // not color-capable → not usable for Ambilight
		}
		id++
		sid := strconv.Itoa(id)
		rev[sid] = l.ID
		v1[sid] = lightV1(l)
	}
	return LightMap{V1: v1, V1ToUUID: rev}
}

// lightV1 builds a single v1 light object from a v2 light, reflecting its real
// capabilities (type + which state fields it exposes) rather than always claiming
// to be a color lamp.
func lightV1(l bridgepro.Light) map[string]any {
	state := map[string]any{
		"on":        l.On.On,
		"alert":     "none",
		"reachable": true,
	}
	if l.HasDimming() {
		state["bri"] = briFromPercent(l.Dimming.Brightness)
	}
	if l.HasColor() && (l.Color.XY.X != 0 || l.Color.XY.Y != 0) {
		state["xy"] = []float64{l.Color.XY.X, l.Color.XY.Y}
		state["colormode"] = "xy"
	}
	if l.HasColorTemperature() && l.ColorTemperature.Mirek != 0 {
		state["ct"] = l.ColorTemperature.Mirek
		if _, hasXY := state["xy"]; !hasXY {
			state["colormode"] = "ct"
		}
	}
	name := l.Metadata.Name
	if name == "" {
		name = "Hue light " + shortID(l.ID)
	}
	typ, modelid, productname := lightProfile(l.HasColor(), l.HasColorTemperature(), l.HasDimming())
	return map[string]any{
		"state":            state,
		"type":             typ,
		"name":             name,
		"modelid":          modelid,
		"manufacturername": "Signify Netherlands B.V.",
		"productname":      productname,
		"uniqueid":         l.ID,
		"swversion":        "1.122.2",
	}
}

// lightProfile maps the bulb's capabilities to the v1 type/modelid/productname.
func lightProfile(color, ct, dim bool) (typ, modelid, productname string) {
	switch {
	case color && ct:
		return "Extended color light", "LCT015", "Hue color lamp"
	case color:
		return "Color light", "LCT015", "Hue color lamp"
	case ct:
		return "Color temperature light", "LTW001", "Hue ambiance lamp"
	case dim:
		return "Dimmable light", "LWB010", "Hue white lamp"
	default:
		return "On/Off plug-in unit", "LOM001", "Hue Smart plug"
	}
}

// shortID returns the first 8 chars of a resource UUID for a fallback light name,
// guarding against a malformed/short id (a v2 UUID is normally 36 chars, but never
// panic the whole light list on a surprising backend value).
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// briFromPercent converts the v2 brightness (0..100 %) into v1 bri (1..254).
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
