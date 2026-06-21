package bridge

import (
	"sync"
	"testing"

	"github.com/trick77/relume-tv/internal/bridgepro"
)

// offFake records the on-state and whether a color was set for every SetLight call.
type offFake struct {
	mu     sync.Mutex
	lights []bridgepro.Light
	steps  []offStep
}

type offStep struct {
	on      bool
	colored bool
}

func (f *offFake) Lights() ([]bridgepro.Light, error) { return f.lights, nil }

func (f *offFake) SetLight(_ string, body map[string]any) error {
	on, _ := body["on"].(map[string]any)
	onVal, _ := on["on"].(bool)
	_, hasColor := body["color"]
	f.mu.Lock()
	f.steps = append(f.steps, offStep{on: onVal, colored: hasColor})
	f.mu.Unlock()
	return nil
}

func TestTurnOffControlled_turnsOffWithoutColor(t *testing.T) {
	// Given
	fc := &offFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}

	// When
	TurnOffControlled(fc, nil, "idle-off", []string{"uuid-1"})

	// Then: a single off-write, no color (no flashing)
	want := []offStep{{on: false, colored: false}}
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

func TestTurnOffControlled_noControlledLightsIsNoop(t *testing.T) {
	fc := &offFake{lights: []bridgepro.Light{{ID: "uuid-1"}}}
	TurnOffControlled(fc, nil, "idle-off", nil) // unknown ambilight set → must not touch any light
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

func TestTurnOffControlled_touchesOnlyTargetUUIDs_notTheWholeHome(t *testing.T) {
	// Given: the Pro has three lights but the TV only drives uuid-2
	fc := &uuidFake{}

	// When
	TurnOffControlled(fc, nil, "shutdown", []string{"uuid-2"})

	// Then: every write targeted uuid-2 only — uuid-1 and uuid-3 are never touched
	fc.mu.Lock()
	hits := fc.hits
	fc.mu.Unlock()
	if len(hits) == 0 {
		t.Fatal("expected a write to the controlled light")
	}
	for _, u := range hits {
		if u != "uuid-2" {
			t.Fatalf("turn-off touched a non-ambilight light %q (hits=%v)", u, hits)
		}
	}
}
