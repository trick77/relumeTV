package translate

import (
	"reflect"
	"testing"
)

func TestStateV1ToV2(t *testing.T) {
	// Given: ein typischer Ambilight-v1-State mit on/bri/xy/transitiontime
	v1 := map[string]any{
		"on":             true,
		"bri":            float64(254),
		"xy":             []any{0.3, 0.4},
		"transitiontime": float64(1),
	}

	// When
	v2 := StateV1ToV2(v1)

	// Then
	if !reflect.DeepEqual(v2["on"], map[string]any{"on": true}) {
		t.Errorf("on = %#v", v2["on"])
	}
	dim := v2["dimming"].(map[string]any)
	if dim["brightness"].(float64) != 100 {
		t.Errorf("brightness = %v, erwartet 100", dim["brightness"])
	}
	col := v2["color"].(map[string]any)["xy"].(map[string]any)
	if col["x"] != 0.3 || col["y"] != 0.4 {
		t.Errorf("xy = %#v", col)
	}
	dyn := v2["dynamics"].(map[string]any)
	if dyn["duration"] != 100 {
		t.Errorf("duration = %v, erwartet 100ms", dyn["duration"])
	}
}

func TestStateV1ToV2_ctOnly(t *testing.T) {
	// Given: nur Weiss-/CT-Steuerung
	v2 := StateV1ToV2(map[string]any{"on": false, "ct": float64(300)})

	// Then
	if _, hasColor := v2["color"]; hasColor {
		t.Error("color sollte nicht gesetzt sein")
	}
	if v2["color_temperature"].(map[string]any)["mirek"] != 300 {
		t.Errorf("mirek falsch: %#v", v2["color_temperature"])
	}
}
