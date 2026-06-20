package entertainment

import (
	"testing"

	"github.com/trick77/relumetv/internal/huestream"
)

func TestToHueV1State_XY(t *testing.T) {
	// XY channel: A=x, B=y (16-bit), C=brightness (full)
	c := huestream.Channel{ID: 6, A: 0xFFFF, B: 0x8000, C: 0xFFFF}
	st := ToHueV1State(huestream.ColorSpaceXY, c)

	if st["on"] != true {
		t.Fatalf("on = %v", st["on"])
	}
	xy, ok := st["xy"].([]float64)
	if !ok || len(xy) != 2 {
		t.Fatalf("xy = %v", st["xy"])
	}
	if xy[0] < 0.999 || xy[0] > 1.0 {
		t.Errorf("x = %v, want ~1.0", xy[0])
	}
	if xy[1] < 0.49 || xy[1] > 0.51 {
		t.Errorf("y = %v, want ~0.5", xy[1])
	}
	if st["bri"] != 254 {
		t.Errorf("bri = %v, want 254", st["bri"])
	}
}

func TestScaleBri_clampsToRange(t *testing.T) {
	if got := scaleBri(0); got != 1 {
		t.Errorf("scaleBri(0) = %d, want 1", got)
	}
	if got := scaleBri(0xFFFF); got != 254 {
		t.Errorf("scaleBri(max) = %d, want 254", got)
	}
}

func TestToHueV1State_RGB_producesValidXY(t *testing.T) {
	// Pure red in RGB colorspace
	c := huestream.Channel{ID: 11, A: 0xFFFF, B: 0x0000, C: 0x0000}
	st := ToHueV1State(huestream.ColorSpaceRGB, c)
	xy := st["xy"].([]float64)
	if xy[0] <= 0 || xy[0] > 1 || xy[1] < 0 || xy[1] > 1 {
		t.Fatalf("xy out of range: %v", xy)
	}
	bri, _ := st["bri"].(int)
	if bri < 1 || bri > 254 {
		t.Fatalf("bri out of range: %d", bri)
	}
}
