package translate

import (
	"testing"

	"github.com/trick77/relume-tv/internal/bridgepro"
)

func colorLight(id, name string) bridgepro.Light {
	var l bridgepro.Light
	l.ID = id
	l.Metadata.Name = name
	l.On.On = true
	l.Dimming = &bridgepro.LightDimming{Brightness: 100}
	l.Color = &bridgepro.LightColor{}
	l.Color.XY.X = 0.5
	l.Color.XY.Y = 0.4
	l.ColorTemperature = &bridgepro.LightColorTemperature{Mirek: 300}
	return l
}

func TestLightsV1_filtersNonColorAndAssignsStableIDs(t *testing.T) {
	// Given: a color bulb, a CT-only bulb and an on/off device (unsorted by UUID)
	color := colorLight("bbbb-2", "Sofa")
	var ctOnly bridgepro.Light
	ctOnly.ID = "aaaa-1"
	ctOnly.Metadata.Name = "Decke"
	ctOnly.Dimming = &bridgepro.LightDimming{Brightness: 50}
	ctOnly.ColorTemperature = &bridgepro.LightColorTemperature{Mirek: 300}
	var plug bridgepro.Light
	plug.ID = "cccc-3"
	plug.Metadata.Name = "Stecker"

	// When (caller passes UUID-sorted order: aaaa, bbbb, cccc)
	lm := LightsV1([]bridgepro.Light{ctOnly, color, plug})

	// Then: only the color bulb is offered, at id 1
	if len(lm.V1) != 1 || lm.V1ToUUID["1"] != "bbbb-2" {
		t.Fatalf("expected only the color bulb at id 1, got %#v", lm.V1ToUUID)
	}
	light1 := lm.V1["1"].(map[string]any)
	if light1["name"] != "Sofa" || light1["type"] != "Extended color light" {
		t.Errorf("light1 = %#v", light1)
	}
	state1 := light1["state"].(map[string]any)
	if state1["colormode"] != "xy" || state1["bri"] != 254 {
		t.Errorf("state wrong: %#v", state1)
	}
}

func TestLightV1_typesByCapability(t *testing.T) {
	cases := []struct {
		name               string
		color, ct, dim     bool
		wantType           string
		wantHasBri, wantXY bool
	}{
		{"color+ct", true, true, true, "Extended color light", true, true},
		{"color only", true, false, true, "Color light", true, true},
		{"ct only", false, true, true, "Color temperature light", true, false},
		{"dimmable", false, false, true, "Dimmable light", true, false},
		{"onoff", false, false, false, "On/Off plug-in unit", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var l bridgepro.Light
			l.ID = "uuid-xxxx"
			l.On.On = true
			if c.dim {
				l.Dimming = &bridgepro.LightDimming{Brightness: 100}
			}
			if c.color {
				l.Color = &bridgepro.LightColor{}
				l.Color.XY.X = 0.4
			}
			if c.ct {
				l.ColorTemperature = &bridgepro.LightColorTemperature{Mirek: 300}
			}
			v := lightV1(l)
			if v["type"] != c.wantType {
				t.Errorf("type = %v, want %v", v["type"], c.wantType)
			}
			state := v["state"].(map[string]any)
			if _, ok := state["bri"]; ok != c.wantHasBri {
				t.Errorf("bri present = %v, want %v", ok, c.wantHasBri)
			}
			if _, ok := state["xy"]; ok != c.wantXY {
				t.Errorf("xy present = %v, want %v", ok, c.wantXY)
			}
		})
	}
}

func TestLightV1_shortIDFallbackNamesDoNotPanic(t *testing.T) {
	// A backend id shorter than 8 chars must not panic the whole light list when
	// the bulb has no name (id[:8] would slice out of range).
	cases := []struct {
		id   string
		want string
	}{
		{"abc", "Hue light abc"},
		{"0123456789abcdef", "Hue light 01234567"},
		{"", "Hue light "},
	}
	for _, c := range cases {
		var l bridgepro.Light
		l.ID = c.id
		l.Color = &bridgepro.LightColor{}
		l.Color.XY.X = 0.4
		v := lightV1(l)
		if v["name"] != c.want {
			t.Errorf("name for id %q = %v, want %v", c.id, v["name"], c.want)
		}
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
