// Package bridge wires the TV-side frontend (clipv1) to the
// Bridge-Pro-side backend (bridgepro) and holds the light mapping.
package bridge

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/translate"
)

// lightCacheTTL limits how often the Bridge Pro is queried for lights.
const lightCacheTTL = 5 * time.Second

// proClient is the subset of *bridgepro.Client the provider needs (read + control),
// extracted so the optimistic control path can be tested without a live Bridge Pro.
type proClient interface {
	Lights() ([]bridgepro.Light, error)
	SetLight(uuid string, v2body map[string]any) error
}

// LightProvider implements clipv1.LightProvider on top of the Bridge Pro and
// holds the v1→UUID mapping for control (REST fallback path).
type LightProvider struct {
	client proClient
	log    *slog.Logger

	// OnControlled, if set, is called with the Bridge Pro UUID of each light the TV
	// drives, on every per-light forward. It feeds the sliding-window ControlledSet
	// so the restart/idle flash and idle-off touch only the bulbs the TV is
	// currently driving, not the whole home. Wired by main.
	OnControlled func(uuid string)

	mu        sync.Mutex
	cached    map[string]any
	v1ToUUID  map[string]string
	fetchedAt time.Time

	// Optimistic REST control: the TV's per-light writes are acknowledged
	// immediately (see SetLightV1) and forwarded to the Bridge Pro asynchronously
	// by drain. pending keeps only the latest state per light, so intermediate
	// Ambilight frames the Bridge Pro cannot keep up with are coalesced away.
	ctrlMu   sync.Mutex
	pending  map[string]map[string]any
	draining bool

	// Forward errors are summarized rather than logged per failed write: the
	// optimistic ack removes the old per-write back-pressure, so a down Bridge Pro
	// would otherwise spam a Warn many times per second (cf. the Ambilight activity
	// summary). errCount accumulates failures between rollups.
	errMu      sync.Mutex
	errCount   int
	lastErrLog time.Time

	// Window stats for the activity rollup, reset by DrainStatsDelta. coalesced
	// counts frames dropped because a newer state for the same light arrived before
	// the Bridge Pro accepted the previous one (the Pro can't keep up); forwardErr
	// counts failed writes to the Pro.
	coalesced  atomic.Uint64
	forwardErr atomic.Uint64
}

// DrainStatsDelta returns the coalesced (dropped) frame count and the forward
// error count since the last call, and resets both. Used by the activity rollup.
func (p *LightProvider) DrainStatsDelta() (coalesced, forwardErrors uint64) {
	return p.coalesced.Swap(0), p.forwardErr.Swap(0)
}

// errLogInterval bounds how often forward failures are logged (a summary with the
// suppressed count), matching the Ambilight activity-summary cadence.
const errLogInterval = 30 * time.Second

// NewLightProvider creates a provider for the given Bridge Pro. log receives the
// asynchronous control-path errors that are no longer surfaced to the TV.
func NewLightProvider(client *bridgepro.Client, log *slog.Logger) *LightProvider {
	return &LightProvider{client: client, log: log, pending: map[string]map[string]any{}}
}

// LightsV1 returns the v1 light list (with a short cache).
func (p *LightProvider) LightsV1() (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Since(p.fetchedAt) < lightCacheTTL {
		return p.cached, nil
	}
	lights, err := p.client.Lights()
	if err != nil {
		return nil, err
	}
	lm := translate.LightsV1(lights)
	p.cached = lm.V1
	p.v1ToUUID = lm.V1ToUUID
	p.fetchedAt = time.Now()
	return p.cached, nil
}

// UUIDForV1 returns the v2 UUID for a numeric v1 light ID.
func (p *LightProvider) UUIDForV1(v1id string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	uuid, ok := p.v1ToUUID[v1id]
	return uuid, ok
}

// SetLightV1 queues the latest state for a light and returns immediately; the
// write is forwarded to the Bridge Pro asynchronously by drain. Acknowledging the
// TV right away keeps its REST control path from blocking on the Bridge Pro
// round-trip — the dominant Ambilight lag. Bridge Pro errors are logged, not
// surfaced to the TV (latency over error reporting). Always returns nil.
func (p *LightProvider) SetLightV1(v1id string, v1state map[string]any) error {
	p.ctrlMu.Lock()
	if _, exists := p.pending[v1id]; exists {
		// A previous frame for this light is still queued → it is dropped (coalesced)
		// because the Bridge Pro has not drained it yet.
		p.coalesced.Add(1)
	}
	p.pending[v1id] = v1state
	if !p.draining {
		p.draining = true
		go p.drain()
	}
	p.ctrlMu.Unlock()
	return nil
}

// drain forwards queued light states to the Bridge Pro until none remain, keeping
// only the latest state per light. It runs in its own goroutine started by
// SetLightV1 and exits once the queue is empty, so a replaced provider's goroutine
// terminates on its own (no writes arrive → drain finishes).
func (p *LightProvider) drain() {
	for {
		p.ctrlMu.Lock()
		if len(p.pending) == 0 {
			p.draining = false
			p.ctrlMu.Unlock()
			return
		}
		batch := p.pending
		p.pending = map[string]map[string]any{}
		p.ctrlMu.Unlock()

		for v1id, v1state := range batch {
			if err := p.forward(v1id, v1state); err != nil {
				p.recordForwardErr(err)
			}
		}
	}
}

// recordForwardErr logs the first failure of a burst immediately, then suppresses
// further ones and emits a summary at most every errLogInterval with the count of
// suppressed failures — so a down Bridge Pro cannot flood the log.
func (p *LightProvider) recordForwardErr(err error) {
	p.forwardErr.Add(1)
	if p.log == nil {
		return
	}
	p.errMu.Lock()
	p.errCount++
	now := time.Now()
	if p.lastErrLog.IsZero() || now.Sub(p.lastErrLog) >= errLogInterval {
		count := p.errCount
		p.errCount = 0
		p.lastErrLog = now
		p.errMu.Unlock()
		p.log.Warn("forwarding lights to bridge pro failing", "failures", count, "last_err", err)
		return
	}
	p.errMu.Unlock()
}

// forward translates a v1 state to v2 and writes it to the Bridge Pro, resolving
// the v1→UUID mapping (loading the light list once if it is not built yet).
func (p *LightProvider) forward(v1id string, v1state map[string]any) error {
	uuid, ok := p.UUIDForV1(v1id)
	if !ok {
		// Mapping may not be built yet → load lights once and check again.
		if _, err := p.LightsV1(); err != nil {
			return err
		}
		if uuid, ok = p.UUIDForV1(v1id); !ok {
			return fmt.Errorf("unknown light id %q", v1id)
		}
	}
	if p.OnControlled != nil {
		p.OnControlled(uuid)
	}
	return p.client.SetLight(uuid, translate.StateV1ToV2(v1state))
}
