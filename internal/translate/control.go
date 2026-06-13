package translate

// StateV1ToV2 übersetzt einen CLIP-v1-Light-State (wie ihn der TV per PUT schickt)
// in einen CLIP-v2-PUT-Body für /clip/v2/resource/light/{id}.
//
// Unterstützte v1-Felder: on, bri (1..254), xy ([x,y]), ct (mirek), hue/sat
// werden bewusst ignoriert — der Ambilight-TV nutzt im REST-Pfad praktisch immer
// xy oder on/bri. transitiontime (in 100ms-Einheiten) wird zu dynamics.duration (ms).
func StateV1ToV2(v1 map[string]any) map[string]any {
	out := map[string]any{}

	if on, ok := v1["on"].(bool); ok {
		out["on"] = map[string]any{"on": on}
	}
	if bri, ok := toFloat(v1["bri"]); ok {
		out["dimming"] = map[string]any{"brightness": briToPercent(bri)}
	}
	if xy, ok := v1["xy"].([]any); ok && len(xy) == 2 {
		x, _ := toFloat(xy[0])
		y, _ := toFloat(xy[1])
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

// briToPercent wandelt v1-bri (1..254) in v2-Helligkeit (0..100 %).
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

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
