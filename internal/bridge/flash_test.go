package bridge

import (
	"sync"
	"testing"
	"time"

	"github.com/trick77/relume/internal/bridgepro"
)

// flashFake records the on/off and color of every SetLight call.
type flashFake struct {
	mu     sync.Mutex
	lights []bridgepro.Light
	steps  []flashStep
}

type flashStep struct {
	on  bool
	red bool
}

func (f *flashFake) Lights() ([]bridgepro.Light, error) { return f.lights, nil }

func (f *flashFake) SetLight(_ string, body map[string]any) error {
	on, _ := body["on"].(map[string]any)
	onVal, _ := on["on"].(bool)
	_, hasColor := body["color"]
	f.mu.Lock()
	f.steps = append(f.steps, flashStep{on: onVal, red: hasColor})
	f.mu.Unlock()
	return nil
}

func TestFlashRestart_blinksRedTwiceThenOff(t *testing.T) {
	// Given: a single light and near-instant blink timings
	defer withFastFlash()()
	fc := &flashFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}

	// When
	FlashRestart(fc, nil)

	// Then: on(red), off, on(red), off — two red blinks ending off
	want := []flashStep{{on: true, red: true}, {on: false}, {on: true, red: true}, {on: false}}
	fc.mu.Lock()
	got := fc.steps
	fc.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("steps = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFlashRestart_noLightsIsNoop(t *testing.T) {
	defer withFastFlash()()
	fc := &flashFake{lights: nil}
	FlashRestart(fc, nil) // must not panic or block
	if len(fc.steps) != 0 {
		t.Fatalf("expected no writes, got %+v", fc.steps)
	}
}

// colorFake records the CIE-x of every on-write so the flash color can be checked.
type colorFake struct {
	mu sync.Mutex
	xs []float64
}

func (f *colorFake) Lights() ([]bridgepro.Light, error) {
	return []bridgepro.Light{{ID: "uuid-1"}}, nil
}

func (f *colorFake) SetLight(_ string, body map[string]any) error {
	if c, ok := body["color"].(map[string]any); ok {
		if xy, ok := c["xy"].(map[string]any); ok {
			if x, ok := xy["x"].(float64); ok {
				f.mu.Lock()
				f.xs = append(f.xs, x)
				f.mu.Unlock()
			}
		}
	}
	return nil
}

func TestFlashIdle_blinksGreenTwiceThenOff(t *testing.T) {
	// Given
	defer withFastFlash()()
	fc := &flashFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}

	// When
	FlashIdle(fc, nil)

	// Then: on(color), off, on(color), off — two blinks ending off
	want := []flashStep{{on: true, red: true}, {on: false}, {on: true, red: true}, {on: false}}
	fc.mu.Lock()
	got := fc.steps
	fc.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("steps = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFlashIdle_usesGreenNotRed(t *testing.T) {
	// Given
	defer withFastFlash()()
	cf := &colorFake{}

	// When
	FlashIdle(cf, nil)

	// Then: every on-write uses the green primary's x (0.217), not red's (0.675)
	cf.mu.Lock()
	xs := cf.xs
	cf.mu.Unlock()
	if len(xs) != flashCount {
		t.Fatalf("color writes = %d, want %d", len(xs), flashCount)
	}
	for _, x := range xs {
		if x != 0.217 {
			t.Fatalf("flash color x = %v, want green 0.217", x)
		}
	}
}

// withFastFlash shrinks the blink durations for the duration of a test.
func withFastFlash() func() {
	on, off := flashOnDur, flashOffDur
	flashOnDur, flashOffDur = time.Millisecond, time.Millisecond
	return func() { flashOnDur, flashOffDur = on, off }
}
