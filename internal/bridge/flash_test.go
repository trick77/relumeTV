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
	on      bool
	colored bool
}

func (f *flashFake) Lights() ([]bridgepro.Light, error) { return f.lights, nil }

func (f *flashFake) SetLight(_ string, body map[string]any) error {
	on, _ := body["on"].(map[string]any)
	onVal, _ := on["on"].(bool)
	_, hasColor := body["color"]
	f.mu.Lock()
	f.steps = append(f.steps, flashStep{on: onVal, colored: hasColor})
	f.mu.Unlock()
	return nil
}

func TestFlashRestart_blinksGreenTwiceThenOff(t *testing.T) {
	// Given: a single light and near-instant blink timings
	defer withFastFlash()()
	fc := &flashFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}

	// When
	FlashRestart(fc, nil, []string{"uuid-1"})

	// Then: on(color), off ×2 — two blinks ending off
	want := []flashStep{
		{on: true, colored: true}, {on: false},
		{on: true, colored: true}, {on: false},
	}
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

func TestFlashRestart_usesGreen(t *testing.T) {
	// Given
	defer withFastFlash()()
	cf := &colorFake{}

	// When
	FlashRestart(cf, nil, []string{"uuid-1"})

	// Then: two on-writes, each the green primary's x (0.217)
	cf.mu.Lock()
	xs := cf.xs
	cf.mu.Unlock()
	if len(xs) != restartFlashCount {
		t.Fatalf("color writes = %d, want %d", len(xs), restartFlashCount)
	}
	for _, x := range xs {
		if x != 0.217 {
			t.Fatalf("restart flash color x = %v, want green 0.217", x)
		}
	}
}

func TestFlashRestart_noControlledLightsIsNoop(t *testing.T) {
	defer withFastFlash()()
	fc := &flashFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}
	FlashRestart(fc, nil, nil) // unknown ambilight set → must not touch any light
	if len(fc.steps) != 0 {
		t.Fatalf("expected no writes when no controlled lights, got %+v", fc.steps)
	}
}

// uuidFake records exactly which light UUIDs SetLight was called for.
type uuidFake struct {
	mu   sync.Mutex
	hits []string
}

func (f *uuidFake) Lights() ([]bridgepro.Light, error) {
	return []bridgepro.Light{{ID: "uuid-1"}, {ID: "uuid-2"}, {ID: "uuid-3"}}, nil
}

func (f *uuidFake) SetLight(uuid string, _ map[string]any) error {
	f.mu.Lock()
	f.hits = append(f.hits, uuid)
	f.mu.Unlock()
	return nil
}

func TestFlash_touchesOnlyTargetUUIDs_notTheWholeHome(t *testing.T) {
	// Given: the Pro has three lights but the TV only drives uuid-2
	defer withFastFlash()()
	fc := &uuidFake{}

	// When
	FlashRestart(fc, nil, []string{"uuid-2"})

	// Then: every write targeted uuid-2 only — uuid-1 and uuid-3 are never touched
	fc.mu.Lock()
	hits := fc.hits
	fc.mu.Unlock()
	if len(hits) == 0 {
		t.Fatal("expected writes to the controlled light")
	}
	for _, u := range hits {
		if u != "uuid-2" {
			t.Fatalf("flash touched a non-ambilight light %q (hits=%v)", u, hits)
		}
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

func TestFlashIdle_blinksTwiceThenOff(t *testing.T) {
	// Given
	defer withFastFlash()()
	fc := &flashFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}

	// When
	FlashIdle(fc, nil, []string{"uuid-1"})

	// Then: on(color), off, on(color), off — two blinks ending off
	want := []flashStep{{on: true, colored: true}, {on: false}, {on: true, colored: true}, {on: false}}
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

func TestFlashIdle_usesBlue(t *testing.T) {
	// Given
	defer withFastFlash()()
	cf := &colorFake{}

	// When
	FlashIdle(cf, nil, []string{"uuid-1"})

	// Then: every on-write uses the blue primary's x (0.167), distinct from the
	// restart flash's green (0.217)
	cf.mu.Lock()
	xs := cf.xs
	cf.mu.Unlock()
	if len(xs) != idleFlashCount {
		t.Fatalf("color writes = %d, want %d", len(xs), idleFlashCount)
	}
	for _, x := range xs {
		if x != 0.167 {
			t.Fatalf("flash color x = %v, want blue 0.167", x)
		}
	}
}

// withFastFlash shrinks the blink durations for the duration of a test.
func withFastFlash() func() {
	on, off := flashOnDur, flashOffDur
	flashOnDur, flashOffDur = time.Millisecond, time.Millisecond
	return func() { flashOnDur, flashOffDur = on, off }
}
