package entertainment

import (
	"math"

	"github.com/trick77/relumetv/internal/huestream"
)

// ToHueV1State converts one decoded HueStream channel into a CLIP v1 light state
// (on/xy/bri) suitable for the REST control path. XY-colorspace channels carry
// x, y and brightness directly; RGB channels are converted to xy + brightness.
func ToHueV1State(colorSpace uint8, c huestream.Channel) map[string]any {
	state := map[string]any{"on": true}
	switch colorSpace {
	case huestream.ColorSpaceXY:
		state["xy"] = []float64{u16ToUnit(c.A), u16ToUnit(c.B)}
		state["bri"] = scaleBri(c.C)
	default: // RGB
		x, y, bri := rgbToXYBri(u16ToUnit(c.A), u16ToUnit(c.B), u16ToUnit(c.C))
		state["xy"] = []float64{x, y}
		state["bri"] = bri
	}
	return state
}

// u16ToUnit maps a 16-bit value to 0..1.
func u16ToUnit(v uint16) float64 { return float64(v) / 65535.0 }

// scaleBri maps a 16-bit brightness to the v1 range 1..254.
func scaleBri(v uint16) int {
	bri := int(float64(v)/65535.0*253.0) + 1
	if bri > 254 {
		return 254
	}
	if bri < 1 {
		return 1
	}
	return bri
}

// rgbToXYBri converts linearizable sRGB (0..1) to CIE xy plus a v1 brightness
// (1..254) from the luminance, using Philips Hue's Wide-RGB D65 matrix.
func rgbToXYBri(r, g, b float64) (x, y float64, bri int) {
	gamma := func(c float64) float64 {
		if c > 0.04045 {
			return math.Pow((c+0.055)/1.055, 2.4)
		}
		return c / 12.92
	}
	r, g, b = gamma(r), gamma(g), gamma(b)
	X := r*0.649926 + g*0.103455 + b*0.197109
	Y := r*0.234327 + g*0.743075 + b*0.022598
	Z := r*0.0 + g*0.053077 + b*1.035763
	sum := X + Y + Z
	if sum == 0 {
		return 0, 0, 1
	}
	bri = int(Y*253.0) + 1
	if bri > 254 {
		bri = 254
	}
	return X / sum, Y / sum, bri
}
