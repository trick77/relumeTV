package translate

// StateV1ToV2 translates a CLIP v1 light state (as the TV sends it via PUT)
// into a CLIP v2 PUT body for /clip/v2/resource/light/{id}.
//
// Supported v1 fields: on, bri (1..254), xy ([x,y]), ct (mirek); hue/sat
// are deliberately ignored — the Ambilight TV practically always uses
// xy or on/bri in the REST path. transitiontime (in 100ms units) becomes dynamics.duration (ms).
func StateV1ToV2(v1 map[string]any) map[string]any {
	out := map[string]any{}

	if on, ok := v1["on"].(bool); ok {
		out["on"] = map[string]any{"on": on}
	}
	if bri, ok := toFloat(v1["bri"]); ok {
		out["dimming"] = map[string]any{"brightness": briToPercent(bri)}
	}
	if x, y, ok := xyPair(v1["xy"]); ok {
		out["color"] = map[string]any{"xy": map[string]any{"x": x, "y": y}}
	}
	if ct, ok := toFloat(v1["ct"]); ok {
		out["color_temperature"] = map[string]any{"mirek": int(ct)}
	}
	if tt, ok := toFloat(v1["transitiontime"]); ok {
		out["dynamics"] = map[string]any{"duration": int(tt) * 100}
	}
	return out
}

// briToPercent converts v1 bri (1..254) into v2 brightness (0..100 %).
func briToPercent(bri float64) float64 {
	if bri <= 1 {
		return 0
	}
	pct := (bri - 1) / 253.0 * 100.0
	if pct > 100 {
		return 100
	}
	return pct
}

// xyPair extracts an [x, y] colour pair from a v1 "xy" value. The TV's JSON REST
// PUT decodes xy as a []any, but the entertainment decode path
// (entertainment.ToHueV1State) builds it as a []float64 — both must be accepted,
// otherwise the colour is silently dropped and the Pro only receives on/bri (the
// lights then keep their last colour). Returns false if xy is missing or malformed.
func xyPair(v any) (x, y float64, ok bool) {
	switch xy := v.(type) {
	case []any:
		if len(xy) != 2 {
			return 0, 0, false
		}
		x, _ = toFloat(xy[0])
		y, _ = toFloat(xy[1])
		return x, y, true
	case []float64:
		if len(xy) != 2 {
			return 0, 0, false
		}
		return xy[0], xy[1], true
	case []float32:
		if len(xy) != 2 {
			return 0, 0, false
		}
		return float64(xy[0]), float64(xy[1]), true
	}
	return 0, 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
