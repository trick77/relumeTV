package bridge

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/relumetv/internal/bridgepro"
)

// downClient models an unreachable Hue Bridge Pro: every write fails, so the provider's
// forward-error path (and the OnForwardErr UI callback) is exercised.
type downClient struct{}

func (downClient) Lights() ([]bridgepro.Light, error)    { return nil, nil }
func (downClient) SetLight(string, map[string]any) error { return errors.New("pro unreachable") }

// fakeClient records the v2 bodies forwarded to the Hue Bridge Pro. The first
// SetLight call blocks on gate (if set), letting a test hold the drain goroutine
// busy while it queues more writes — to observe coalescing deterministically.
type fakeClient struct {
	mu    sync.Mutex
	mirek []int
	gate  chan struct{}
	n     int
}

func (f *fakeClient) Lights() ([]bridgepro.Light, error) { return nil, nil }

func (f *fakeClient) SetLight(_ string, body map[string]any) error {
	f.mu.Lock()
	first := f.n == 0
	f.n++
	f.mu.Unlock()
	if first && f.gate != nil {
		<-f.gate
	}
	ct, _ := body["color_temperature"].(map[string]any)
	f.mu.Lock()
	f.mirek = append(f.mirek, ct["mirek"].(int))
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) seen() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.mirek...)
}

func (f *fakeClient) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func newTestProvider(c proClient) *LightProvider {
	p := &LightProvider{client: c, pending: map[string]map[string]any{}}
	p.v1ToUUID = map[string]string{"1": "uuid-1", "2": "uuid-2"} // skip the Lights() resolution
	p.uuidToV1 = map[string]string{"uuid-1": "1", "uuid-2": "2"} // inverse, as LightsV1 builds it
	return p
}

func TestV1ForUUID_isInverseOfUUIDForV1(t *testing.T) {
	p := newTestProvider(&fakeClient{})

	if v1, ok := p.V1ForUUID("uuid-2"); !ok || v1 != "2" {
		t.Fatalf("V1ForUUID(uuid-2) = %q,%v want 2,true", v1, ok)
	}
	// Round-trips with the forward map.
	if uuid, _ := p.UUIDForV1("1"); uuid != "uuid-1" {
		t.Fatalf("UUIDForV1(1) = %q want uuid-1", uuid)
	}
	if _, ok := p.V1ForUUID("uuid-unknown"); ok {
		t.Fatalf("V1ForUUID(unknown) ok = true, want false")
	}
}

func TestOnControlled_firesOnEveryForward(t *testing.T) {
	// Given: a provider that reports which UUIDs the TV drives
	fc := &fakeClient{}
	p := newTestProvider(fc)
	var mu sync.Mutex
	var got []string
	p.OnControlled = func(uuid string) {
		mu.Lock()
		got = append(got, uuid)
		mu.Unlock()
	}

	// When: light 1 is driven twice and light 2 once
	_ = p.forward("1", map[string]any{"ct": 200})
	_ = p.forward("1", map[string]any{"ct": 200})
	_ = p.forward("2", map[string]any{"ct": 200})

	// Then: it fires on every forward (so the sliding window keeps being refreshed)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "uuid-1" || got[1] != "uuid-1" || got[2] != "uuid-2" {
		t.Fatalf("OnControlled calls = %v, want [uuid-1 uuid-1 uuid-2]", got)
	}
}

func TestForward_emptyV2StateSkipsWriteAndControlled(t *testing.T) {
	// Given: a provider that reports which UUIDs the TV drives
	fc := &fakeClient{}
	p := newTestProvider(fc)
	var mu sync.Mutex
	var controlled int
	p.OnControlled = func(string) {
		mu.Lock()
		controlled++
		mu.Unlock()
	}

	// When: a state that translates to an empty v2 body (e.g. a group action carrying
	// only non-light-state keys like "scene", which StateV1ToV2 drops)
	if err := p.forward("1", map[string]any{"scene": "abc"}); err != nil {
		t.Fatalf("forward: %v", err)
	}

	// Then: nothing is written to the Pro and the light is NOT marked controlled
	mu.Lock()
	gotControlled := controlled
	mu.Unlock()
	if fc.calls() != 0 {
		t.Errorf("SetLight calls = %d, want 0 for an empty-yielding state", fc.calls())
	}
	if gotControlled != 0 {
		t.Errorf("OnControlled calls = %d, want 0 for an empty-yielding state", gotControlled)
	}

	// And: a real state still forwards and marks controlled
	if err := p.forward("1", map[string]any{"ct": 200}); err != nil {
		t.Fatalf("forward (real): %v", err)
	}
	mu.Lock()
	gotControlled = controlled
	mu.Unlock()
	if fc.calls() != 1 || gotControlled != 1 {
		t.Errorf("after real state: SetLight=%d controlled=%d, want 1/1", fc.calls(), gotControlled)
	}
}

func TestForward_failedWriteDoesNotMarkControlled(t *testing.T) {
	// Given: a provider whose Pro write always fails (down/overloaded)
	p := newTestProvider(downClient{})
	var controlled, colored int
	p.OnControlled = func(string) { controlled++ }
	p.OnColor = func(string, map[string]any) { colored++ }

	// When: a real state is forwarded but the Pro rejects the write
	err := p.forward("1", map[string]any{"ct": 200})

	// Then: the error propagates and the light is NEITHER marked controlled (so the
	// restart/idle flash won't target a bulb the TV never actually drove) NOR surfaced to
	// the UI as a live colour it never received.
	if err == nil {
		t.Fatal("forward: expected an error from a failed write, got nil")
	}
	if controlled != 0 {
		t.Errorf("OnControlled calls = %d, want 0 on a failed write", controlled)
	}
	if colored != 0 {
		t.Errorf("OnColor calls = %d, want 0 on a failed write", colored)
	}
}

func TestSetLightV1_isAsyncAndForwards(t *testing.T) {
	// Given: a provider over a fake Hue Bridge Pro
	fc := &fakeClient{}
	p := newTestProvider(fc)

	// When: a single write
	if err := p.SetLightV1("1", map[string]any{"ct": 200}); err != nil {
		t.Fatalf("SetLightV1 returned error: %v", err)
	}

	// Then: it is eventually forwarded to the Hue Bridge Pro (asynchronously)
	waitFor(t, func() bool { return len(fc.seen()) == 1 })
	if got := fc.seen(); got[0] != 200 {
		t.Fatalf("forwarded mirek = %v, want [200]", got)
	}
}

func TestSetLightV1_coalescesToLatestPerLight(t *testing.T) {
	// Given: the first forward is held in flight so writes pile up behind it
	gate := make(chan struct{})
	fc := &fakeClient{gate: gate}
	p := newTestProvider(fc)

	// When: the first write starts the drain and blocks inside SetLight
	p.SetLightV1("1", map[string]any{"ct": 1})
	waitFor(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.n == 1 // drain goroutine is now blocked in the first SetLight
	})
	// ...and several more states queue while the Hue Bridge Pro is busy
	for _, v := range []int{2, 3, 4, 5} {
		p.SetLightV1("1", map[string]any{"ct": v})
	}

	// Then: releasing the gate forwards only the first and the latest — the
	// intermediate frames (2,3,4) are coalesced away.
	close(gate)
	waitFor(t, func() bool { return len(fc.seen()) == 2 })
	got := fc.seen()
	if len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("forwarded mirek = %v, want [1 5] (intermediate frames coalesced)", got)
	}
}

// OnCoalesce fires once per frame the optimistic path drops (a newer state for the
// same light arriving before the Pro accepted the previous one) — the drops/s the
// Backpressure card shows. Mirrors the coalescing test, but counts the callback.
func TestSetLightV1_firesOnCoalesce(t *testing.T) {
	// Given: the first forward is held in flight so writes pile up behind it
	gate := make(chan struct{})
	fc := &fakeClient{gate: gate}
	p := newTestProvider(fc)
	var coalesced atomic.Int64
	p.OnCoalesce = func() { coalesced.Add(1) }

	// When: the first write blocks the drain, then more states for the SAME light queue
	p.SetLightV1("1", map[string]any{"ct": 1})
	waitFor(t, func() bool {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		return fc.n == 1 // drain is now blocked inside the first SetLight
	})
	for _, v := range []int{2, 3, 4, 5} {
		p.SetLightV1("1", map[string]any{"ct": v})
	}

	// Then: write 2 found the queue empty (drained), writes 3,4,5 each replaced a
	// still-pending state → three coalesced drops.
	if got := coalesced.Load(); got != 3 {
		t.Fatalf("OnCoalesce fired %d times, want 3 (one per dropped frame)", got)
	}
	close(gate) // let the drain goroutine finish
}

// OnForwardErr fires once per failed REST write to the Pro — the cumulative error
// count the Backpressure card shows (distinct from the healthy coalesce drops).
func TestSetLightV1_firesOnForwardErr(t *testing.T) {
	// Given: a provider whose every write to the Pro fails (Pro unreachable)
	p := newTestProvider(downClient{})
	var errs atomic.Int64
	p.OnForwardErr = func() { errs.Add(1) }

	// When: a write is queued and the async drain tries (and fails) to forward it
	if err := p.SetLightV1("1", map[string]any{"ct": 200}); err != nil {
		t.Fatalf("SetLightV1 returned error: %v", err)
	}

	// Then: OnForwardErr fires for the failed Pro write
	waitFor(t, func() bool { return errs.Load() == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
