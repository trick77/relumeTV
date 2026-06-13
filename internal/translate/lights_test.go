package translate

import (
	"testing"

	"github.com/trick77/relume/internal/bridgepro"
)

func TestLightsV1_mapsAndAssignsStableIDs(t *testing.T) {
	// Given: zwei v2-Lampen (unsortiert nach UUID)
	var a, b bridgepro.Light
	a.ID = "bbbb-2"
	a.Metadata.Name = "Sofa"
	a.On.On = true
	a.Dimming.Brightness = 100
	a.Color.XY.X = 0.5
	a.Color.XY.Y = 0.4
	b.ID = "aaaa-1"
	b.Metadata.Name = "Decke"
	b.ColorTemperature.Mirek = 300

	// When: LightsV1 erwartet bereits sortierte Eingabe (Client liefert das);
	// hier simulieren wir die sortierte Reihenfolge aaaa, bbbb.
	lm := LightsV1([]bridgepro.Light{b, a})

	// Then: numerische IDs 1,2 zeigen stabil auf die UUIDs
	if lm.V1ToUUID["1"] != "aaaa-1" || lm.V1ToUUID["2"] != "bbbb-2" {
		t.Fatalf("mapping falsch: %#v", lm.V1ToUUID)
	}
	light1 := lm.V1["1"].(map[string]any)
	if light1["name"] != "Decke" {
		t.Errorf("name = %v", light1["name"])
	}
	state1 := light1["state"].(map[string]any)
	if state1["colormode"] != "ct" || state1["ct"] != 300 {
		t.Errorf("ct-state falsch: %#v", state1)
	}

	light2 := lm.V1["2"].(map[string]any)
	state2 := light2["state"].(map[string]any)
	if state2["colormode"] != "xy" {
		t.Errorf("colormode = %v, erwartet xy", state2["colormode"])
	}
	if state2["bri"] != 254 {
		t.Errorf("bri = %v, erwartet 254 (100%%)", state2["bri"])
	}
}

func TestBriFromPercent(t *testing.T) {
	cases := []struct {
		pct  float64
		want int
	}{{0, 1}, {100, 254}, {50, 127}}
	for _, c := range cases {
		if got := briFromPercent(c.pct); got != c.want {
			t.Errorf("briFromPercent(%v) = %d, want %d", c.pct, got, c.want)
		}
	}
}
