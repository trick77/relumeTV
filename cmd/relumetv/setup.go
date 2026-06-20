package main

import (
	"log/slog"
	"sync"

	"github.com/trick77/relumetv/internal/config"
)

// The setup wizard's six steps. The state machine advances monotonically: a step is
// left only when its own completion signal fires, never derived wholesale from a
// snapshot (steps 3 and 5 are transitions, not stateless predicates — see recompute).
const (
	stepPairPro     = 1 // pair the Hue Bridge Pro (proPaired)
	stepProPowerOff = 2 // disconnect the Pro from power (reachable -> unreachable)
	stepRebootTV    = 3 // reboot the TV (its UA fetches /description.xml)
	stepTVScan      = 4 // the TV scans and pairs with relumeTV (tvClients > 0)
	stepProPowerOn  = 5 // power the Pro back on (reachable again)
	stepAssignBulbs = 6 // assign bulbs / first TV data drives the lights
	stepDone        = 7 // setup complete (config committed)

	setupSteps = 6
)

// setupTitles are the short, headless-log titles per step, so a -headless run logs
// each transition as "setup step N/6: <title>" with no web UI.
var setupTitles = map[int]string{
	stepPairPro:     "pair the Hue Bridge Pro",
	stepRebootTV:    "reboot your TV",
	stepProPowerOff: "disconnect the Hue Bridge Pro from power",
	stepTVScan:      "start the relumeTV scan in the TV's Ambilight+Hue settings",
	stepProPowerOn:  "turn the Hue Bridge Pro back on",
	stepAssignBulbs: "assign your color bulbs in the TV's Ambilight+Hue menu",
}

// setupStatus is the backend state machine that drives the setup wizard. It owns the
// monotone step counter (1..6 then stepDone) the UI renders, the discovery
// preconditions, the Pro reachability and the TV descriptor signal — all behind one
// mutex. It is the single source of truth for BOTH the web UI (a pure renderer of
// currentStep) and headless operation (every transition is logged), so a transition
// survives an SSE reconnect/refresh and the -headless path uses the exact same machine.
//
// Steps 3 and 5 are transitions, not stateless predicates: step 3 advances only after
// the Pro goes from reachable to unreachable (everReachable latches the first
// confirmed-up so it cannot fire at t=0), and step 5 is evaluated only after step 3 has
// completed (monotone gating), so neither flickers when a recompute runs while the Pro
// is up.
type setupStatus struct {
	mu sync.Mutex

	step int

	// Preconditions, set by autoPairPro from the discovery outcome:
	//   discoveryOK=false        -> (c) mDNS discovery unavailable
	//   discoveredHost==""       -> (a) no bridge found
	//   bridgeIsPro==false       -> (b) the found bridge is not a Pro
	discoveryOK    bool
	discoveredHost string
	bridgeIsPro    bool
	precondMsg     string

	// proReachable is the latest reachability from the proWatcher tick. everReachable
	// latches the first confirmed-up so step 3 (reachable -> unreachable) can never fire
	// before the Pro was ever seen up (no t=0 flicker).
	proReachable  bool
	everReachable bool

	// tvDescriptorSeen latches a /description.xml fetch by the TV User-Agent — but only
	// while step 2 is the active step (see markTVDescriptorSeen). A pre-step-2 probe (the
	// TV polls the descriptor during ordinary Hue discovery) must not skip the reboot.
	tvDescriptorSeen bool

	committed bool // the one-shot Commit has run

	cfg      *config.Config
	active   func() bool  // the TV is driving the lights right now (step 6 signal)
	commit   func() error // persists the config once (cfg.Commit)
	onChange func()       // pushes a fresh SSE snapshot promptly after a change
	log      *slog.Logger
}

// newSetupStatus builds the machine. A config loaded from an existing file is already
// committed (a prior setup finished), so the machine starts at stepDone and the UI
// shows the dashboard, never the wizard.
func newSetupStatus(cfg *config.Config, active func() bool, commit func() error, log *slog.Logger) *setupStatus {
	s := &setupStatus{
		step:        stepPairPro,
		discoveryOK: true, // optimistic until autoPairPro reports otherwise
		cfg:         cfg,
		active:      active,
		commit:      commit,
		log:         log,
	}
	if cfg.Committed() {
		s.step = stepDone
		s.committed = true
	}
	return s
}

// setOnChange registers the SSE push callback. Separate from the constructor because
// the hub/source wiring is only available later in serve startup (and absent headless).
func (s *setupStatus) setOnChange(f func()) { s.onChange = f }

// CurrentStep returns the active wizard step (1..6, or stepDone) for the snapshot.
func (s *setupStatus) CurrentStep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.step
}

// Precond returns the discovery precondition state for the snapshot/UI banner.
func (s *setupStatus) Precond() (discoveryOK bool, discoveredHost string, bridgeIsPro bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discoveryOK, s.discoveredHost, s.bridgeIsPro, s.precondMsg
}

// ProReachable reports the latest Pro reachability for the snapshot.
func (s *setupStatus) ProReachable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proReachable
}

// TVDescriptorSeen reports whether the TV has fetched the descriptor (step 2 signal).
func (s *setupStatus) TVDescriptorSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tvDescriptorSeen
}

// setPrecond records the discovery precondition outcome from autoPairPro and pushes a
// snapshot. msg is empty on success.
func (s *setupStatus) setPrecond(discoveryOK bool, discoveredHost string, bridgeIsPro bool, msg string) {
	s.mu.Lock()
	s.discoveryOK = discoveryOK
	s.discoveredHost = discoveredHost
	s.bridgeIsPro = bridgeIsPro
	s.precondMsg = msg
	s.mu.Unlock()
	s.notifyChange()
}

// setReachable feeds the Pro reachability from the watcher tick. Called every tick (not
// only on change) so the level-read steps (4 tvPaired, 6 active) are re-evaluated each
// time. A confirmed-up latches everReachable so step 3 can detect the later down edge.
func (s *setupStatus) setReachable(reachable bool) {
	s.mu.Lock()
	if reachable {
		s.everReachable = true
	}
	s.proReachable = reachable
	s.recompute()
	s.mu.Unlock()
	s.notifyChange()
}

// markTVDescriptorSeen latches the step-2 signal — but ONLY while step 2 is active, so a
// descriptor fetch the TV makes during ordinary Hue discovery (before the user reboots)
// cannot skip the reboot step. Wired to clipv1's OnDescriptorFetch.
func (s *setupStatus) markTVDescriptorSeen() {
	s.mu.Lock()
	if s.step == stepRebootTV {
		s.tvDescriptorSeen = true
		s.recompute()
	}
	s.mu.Unlock()
	s.notifyChange()
}

// recomputeNow re-evaluates the machine after an external signal changed (e.g. the Pro
// was paired). Safe to call from any goroutine.
func (s *setupStatus) recomputeNow() {
	s.mu.Lock()
	s.recompute()
	s.mu.Unlock()
	s.notifyChange()
}

// recompute advances the machine as far as the current signals allow. Caller holds mu.
// Each step leaves only on its own signal; the loop lets several queued signals advance
// in one pass without ever skipping a step's gate.
func (s *setupStatus) recompute() {
	for s.step < stepDone {
		proPaired := s.cfg.GetPro() != nil
		tvPaired := len(s.cfg.PairedDeviceTypes()) > 0

		next := s.step
		switch s.step {
		case stepPairPro:
			if proPaired {
				next = stepProPowerOff
			}
		case stepProPowerOff:
			// Transition, latched: the Pro must have been seen up and now be down.
			if s.everReachable && !s.proReachable {
				next = stepRebootTV
			}
		case stepRebootTV:
			if s.tvDescriptorSeen {
				next = stepTVScan
			}
		case stepTVScan:
			if tvPaired {
				next = stepProPowerOn
			}
		case stepProPowerOn:
			// Only evaluated after the power-off step completed (monotone gating): the Pro is back.
			if s.proReachable {
				next = stepAssignBulbs
			}
		case stepAssignBulbs:
			if s.active != nil && s.active() {
				next = stepDone
			}
		}
		if next == s.step {
			return
		}
		s.step = next
		if next == stepDone {
			s.log.Info("setup complete — TV data flowing, persisting config")
			s.doCommit()
			return
		}
		s.log.Info("setup step", "step", next, "of", setupSteps, "do", setupTitles[next])
	}
}

// doCommit persists the config exactly once at setup completion. Caller holds mu; the
// lock order is setupStatus.mu -> cfg.mu (consistent with recompute's cfg reads), and
// no path holds cfg.mu before entering setupStatus, so this cannot deadlock.
func (s *setupStatus) doCommit() {
	if s.committed {
		return
	}
	s.committed = true
	if s.commit == nil {
		return
	}
	if err := s.commit(); err != nil {
		s.log.Error("persisting config at setup completion", "err", err)
		return
	}
	s.log.Info("config saved")
}

// notifyChange pushes a fresh SSE snapshot if a sink is wired (no-op headless).
func (s *setupStatus) notifyChange() {
	if s.onChange != nil {
		s.onChange()
	}
}
