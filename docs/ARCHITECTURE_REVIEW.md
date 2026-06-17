# relume — Architecture Review

Date: 2026-06-17 · Branch: `review/architecture-review` · Scope: full `internal/*` + `cmd/relume` + build/CI.
Security is **out of scope** by request (the bridge/container runs LAN-only, no external access). This review
covers structure, coupling, concurrency correctness, error handling, abstractions, testability and maintainability.

## Verdict

This is a well-structured small Go codebase. The layering is clean (no import cycles, `config` is a proper leaf,
`clipv1` uses textbook dependency inversion), the hard-won protocol knowledge is preserved as deliberate
invariants rather than lost, and the trickiest mechanisms — the coalescing optimistic provider, the REST/DTLS
mutual exclusion, the sticky fallback watchdog — are sound in their core design and unusually well-commented.

The weaknesses cluster in four places: (1) one real concurrency bug in the composition root, (2) experimental
discovery-debugging scaffolding that has solidified into permanent production surface area, (3) two large
"god" units (`clipv1.Server`, `runServe`) with no test seam around the orchestration that matters most, and
(4) a coarse error model that forces the resilience logic to be blunter than it should be.

---

## What is well-designed (keep)

- **Layering & dependency inversion.** No import cycles. `clipv1` declares its own `LightProvider` interface and
  receives behavior via function fields (`ForwardLight`, `ControlledLights`, `MarkActivity`); all cross-layer
  wiring is closures in `main.go`. The HTTP server is testable in isolation — and well tested (35 tests).
- **Runtime provider hot-swap.** `LightProvider` + RWMutex (`clipv1/server.go:30-35,226-247`) cleanly models the
  async-Pro-pairing lifecycle with nil-safe fallback. The capability interface `drainStatsProvider` surfaces
  backend stats without coupling.
- **Translation layer.** `internal/translate` is pure functions over `bridgepro` value types — no I/O, no client
  coupling — fully unit-tested including the `[]any`/`[]float64`/`[]float32` xy variants that caused the real
  "stuck red" bug. Capability modeling via pointer sub-objects (`resources.go:56-102`) correctly distinguishes
  "absent" from "zero".
- **Coalescing optimistic provider** (`bridge/provider.go`): the latest-state-wins drain with a self-terminating
  goroutine (dies when a provider is swapped out) is a genuinely good fit for the rate mismatch, and well tested.
- **mDNS no-goodbye discipline** (`mdns/announce.go:80-112`): a captured bug encoded as a deliberate non-action
  with thorough rationale. Exemplary.
- **Atomic config writes + copy-on-write `SetPro`** (`config.go:217-229`): the design intent is correct.
  `BridgePro.LogValue` for secret-free structured logging is a nice detail.
- **Graceful shutdown ordering** is thoughtfully sequenced and documented (`main.go:261-263, 379-397`).

---

## Findings (prioritized)

### H1 — Data race on `cfg.Pro` direct field access  ·  *High · Low effort*
`cmd/relume/main.go:166, 212-215, 240, 276-283, 394, 546` read the exported `cfg.Pro` field directly, bypassing
the mutex, while `autoPairPro`/`watchPro` mutate it concurrently via `cfg.SetPro` (`config.go:224`). The type was
*deliberately* built for safe concurrent access — `GetPro()` locks, `SetPro` does copy-on-write — and
`monitorIdle` correctly uses `cfg.GetPro()` (`main.go:457`). But `watchPro` reads `pro := cfg.Pro` unguarded
(`:546`) and the shutdown path (`:394`) and identity log (`:166`) race the pairing goroutines. The `-race` suite
misses it only because no test drives the concurrent serve path.
**Fix:** route every read through `GetPro()`. Additionally `watchPro` mutates the returned pointer in place
(`main.go:552-558`: `pro.Name, pro.BridgeID = …`), violating the immutability contract `GetPro` documents — build
a fresh `*BridgePro` and `SetPro` it, as `reconnectProConfig` (`:607-617`) already does correctly.

### H2 — Experimental / diagnostic scaffolding is now permanent production surface area  ·  *High · Medium effort*
The discovery-debugging A/B machinery has outlived its experiments (M1 discovery and M4 entertainment are both
VERIFIED per AGENTS.md), but it remains first-class API threaded through flags, struct fields, branches and tests:
- Identity/description profiles, MediaServer alias/basic-body, descriptor variants, the `?relume=` LOCATION hack:
  `clipv1/server.go:48-68,532-558,639-648`, `ssdp/ssdp.go:33-48,249-279`, `upnp/description.go:18-23,86-106`,
  wired by ~5 `serve` flags (`main.go:94-139`) into `serveOptions` and `config.go:51-61`.
- `RELUME_ENT_PROBE` passive probe (`diag/entprobe.go`, `main.go:185`) — explicitly "superseded by
  `-mode entertainment`"; duplicates the receiver's `udp4 :2100` bind.
- `RELUME_PUT_TRACE` httptrace plumbing inside the hot `put` path (`bridgepro/client.go:27-31,210-245`) — its own
  comment says "Remove once the Ambilight lag is diagnosed", which has happened.

This widens the surface on every invocation and forces multi-dimensional branching in `handleDescription`,
`ssdpVariants`, etc. **Fix:** retire the profile/alias/probe/trace machinery now the verified identity is known
(collapse `Render`/`RenderWithProfile`/`RenderWithOptions` to one path), or gate it behind a single
`-experimental` flag. Flag before deleting per repo norms.

### H3 — `clipv1.Server` is a god-object; entertainment/DTLS state split across 3 sync primitives  ·  *High · Medium effort*
`clipv1/server.go:38-124` accretes ~6 concerns on one struct with **five** mutexes (`activityMu`, `streamMu`,
`dtlsMu`, `pairMu`, `lightsMu`): HTTP identity, the light provider, activity metrics, entertainment stream state,
the DTLS-fallback state machine, and pairing-delay state. The fallback invariant "fell-back ⇒ stream inactive"
is maintained by convention across `streamMu` (`streamActive`/`streamOwner`), two `atomic.Bool`
(`dtlsFallback`/`dtlsStreamUp`) and `dtlsMu` (the timer) — `dtlsWatchdogFired` (`:210-221`) and `handleGroupUpdate`
(`:863-888`) lock them in opposite groupings, so a reader can observe a mid-transition state. Benign today (one
stale `GET /groups/1`), fragile by design. **Fix:** extract `activityTracker`, `streamState`/`dtlsWatchdog`,
`pairingGate` into small types each owning one mutex; fold the entertainment/DTLS state behind one lock with
explicit transition methods.

### M1 — Sticky fallback can permanently disable entertainment mode on an unlucky timing  ·  *Medium · Low effort*
`clipv1/server.go:194-221`. `MarkDTLSStreamUp` does `Store(true)` then `disarmDTLSWatchdog()`, while
`dtlsWatchdogFired` reads `dtlsStreamUp` first. A watchdog fire that began before the `Store` proceeds to set
`dtlsFallback=true` even though the TV's stream is coming up — and fallback is **process-sticky** (`:101-102`), so
one unlucky race on a *healthy* TV disables DTLS until restart. **Fix:** clear `dtlsFallback` in
`MarkDTLSStreamUp`, or re-check after a short grace, or make fallback per-activation rather than per-process.

### M2 — `ProStreamer.Start`/`Stop` are not serialized; `Stop` never joins the `run` goroutine  ·  *Medium · Medium effort*
`entertainment/streamer.go:96-99,127-150,241-254,292-296`. `Stop` cancels and sets `running=false` but does not
wait for `run` to exit. On a fast TV reconnect (OnStreamStop→Stop→OnStreamStart→Start) the old `run` goroutine can
still be in `establish` and overwrite `s.st.conn`/`path` *after* a new `Start` began → two goroutines driving
`s.st`, one orphaned DTLS conn never closed. Compounded by `dialPro` using `context.Background()` with its own 10s
timeout (`:593`) instead of the run ctx, so a `Stop` during the handshake can't cancel the in-flight dial.
**Fix:** give `run` a done-channel `Stop` waits on (makes Start/Stop serial and the post-stop state testable);
thread the run ctx into `dialPro`'s `HandshakeContext`.

### M3 — Coarse, string-only error model forces blunt resilience  ·  *Medium · Medium effort*
`bridgepro/client.go:181-198,247-257,276-284`. Every error is an untyped `fmt.Errorf`. The 503 "command queue is
full" (the entire motivation for entertainment mode) is indistinguishable from auth failure, unreachable host, or
a per-attribute `color.xy` rejection. Consequences: `provider.go:157` can only *count* failures, not back off on
503; `watchPro` (`main.go:560-562`) treats *any* `Lights()` error as "Pro unreachable → re-discover + re-pin",
firing cloud discovery and a cert re-fetch on a transient 500 or JSON blip. The same triplicated anonymous
`Errors[]` decode also has an inconsistent policy (`post` hard-errors on decode failure, `put`/`del` swallow it).
**Fix:** small typed error set (`ErrQueueFull`, `ErrUnreachable`, `ErrDomain`) + one `decodeCLIPErrors` helper.

### M4 — `runServe` is a ~250-line orchestration with no test seam  ·  *Medium · Medium effort*
`cmd/relume/main.go:141-398` does flag validation, IP detection, mode selection, provider/SSDP/mDNS/HTTP
construction, 5+ goroutine launches, the entertainment/probe/rest branch, and shutdown/flash inline. Only the
extracted pure helpers are tested; the wiring, ordering and shutdown have zero coverage. The most failure-prone
code (`autoPairPro`/`watchPro` re-discovery, re-pin, hot-swap) is untested because `*bridgepro.Client` is taken
concretely everywhere except the `proClient` interface in `provider.go:21-24` — there's no seam to inject a fake.
**Fix:** extract an `application` struct (build deps → `Run(ctx)`); define the Pro-client interface in `bridgepro`
so the resilience helpers are unit-testable.

### M5 — Duplicated Pro-pairing sequence (×3)  ·  *Medium · Medium effort*
Discover → `FetchLeafFingerprint` → `HTTPClientFor` → `Pair` → `BridgeInfo` → `SetPro` → list lights is
implemented nearly verbatim in `runSetup` (`main.go:680-754`) and `autoPairPro` (`:477-537`), with `watchPro`
(`:560-601`) repeating the discover+fingerprint+reconnect half. This is the most correctness-sensitive code
(cert pinning, key persistence) living in three copies in the entrypoint. **Fix:** extract a
`Pairer`/`Reconnector` into `bridgepro`.

### M6 — Re-discovery picks `bridges[0]`; re-pin is trust-on-every-reconnect  ·  *Medium · Medium effort*
`autoPairPro:482` and `watchPro:571` blindly take `bridges[0].InternalIPAddress` with no filter by the *paired*
bridge id (`config.BridgePro` doesn't even persist the discovery `id`, though `DiscoveredBridge.ID` is parsed). On
the documented coexistence LAN (a real BSB003 present) this can re-point at the wrong bridge, then
`watchPro:579-587` unconditionally overwrites the pin with whatever cert that IP presents — defeating the point of
pinning across reconnects. LAN-only ⇒ low risk, but a resilience correctness smell. **Fix:** persist & match the
Pro's discovery id; re-pin only if the surviving appKey still authenticates against the new cert.

### M7 — `handleGroupAction` silently drops Ambilight frames  ·  *Medium · Medium effort*
`clipv1/server.go:836-844`. `PUT /groups/{id}/action` is logged and acked but never forwarded. If a TV/firmware
ever drives lights via the group path, lights silently won't follow while relume reports success — the
`recordGroupActionWrite` tally exists precisely to detect this. **Fix:** forward through the same provider, or
document why it's safe to ignore for the target TV.

### Low-severity findings

- **L1 — Triplicated `interfaceForIP`** (`ssdp/ssdp.go:147-171`, `mdns/announce.go:135-159`, `diag/mdns.go:70`),
  byte-identical → extract to a shared `internal/netutil`. *Low effort.*
- **L2 — No bounded queue between DTLS decode and forward** (`entertainment/receiver.go:143-144`): `OnFrame`/`Push`
  run synchronously on the single reader goroutine. Safe only because every callback is currently non-blocking; a
  future slow `fallback` would stall intake with silent UDP drop and no backpressure. Document as a hard invariant
  or add a select/drop stage.
- **L3 — `OnFrame` and `OnStreamStop` are unordered** (`receiver.go`): a late frame can re-`StartStream` race the
  stop. Currently benign.
- **L4 — Shutdown flash has no total deadline** (`main.go:394-396`, `flash.go:75-80`): if the Pro is unreachable at
  shutdown, `FlashRestart` blocks on per-call HTTP timeouts until container SIGKILL. Wrap in a bounded deadline.
- **L5 — Config has no schema version / no fsync / orphaned `.tmp` on rename failure** (`config.go:108-122,253-269`).
  Additive evolution works today; a rename or restructure has no migration path, and a garbage file fails hard with
  no quarantine. Add a `schemaVersion` field now.
- **L6 — `health check = Lights()`** (`watchPro:561`): a heavy call used as liveness probe; `BridgeInfo` is the
  lighter ping already available.
- **L7 — Unguarded slices / narrow type handling**: `lights.go:65` (`l.ID[:8]` panics on short id);
  `control.go:53-55` `xyPair` returns `ok=true` on failed component conversion; `control.go:70-78` `toFloat` omits
  `int64` (same bug class as "stuck red", reachable via the in-process entertainment `bri` path); ignored
  `json.Marshal` errors (`client.go:114,168,206`); v2 channel id truncated to 8 bits without a `<=255` guard
  (`huestream.go:66,124`). All low-risk, all defensive nits.
- **L8 — Containerfile comments are German** (`Containerfile:1,11,17-18`) — violates the repo's own "all content
  English" rule (AGENTS.md). *Trivial.*
- **L9 — DTLS vs REST color paths differ**: the DTLS path passes color verbatim while the REST fallback applies
  gamma (`convert.go`/`streamer.go:165-176`) — falling back mid-session visibly shifts on-screen color. By design;
  worth a note in DESIGN.md's fallback section.
- **L10 — `release.yaml` ships a patch release on every push to master** with no tag gate or changelog. Reasonable
  for a hobby project — worth a conscious decision.

---

## Test coverage map

| Area | Tested? |
|---|---|
| `translate/*`, `bridge/provider.go`, `clipv1` (incl. watchdog state machine), `upnp`, `bridgepro/entertainment.go` | **Yes — good** |
| `bridgepro/client.go` (get/post/put/del, **207 multi-status**, cert pin), `resources.go`, `discover.go` | **No** (H-priority gap) |
| `cmd/relume` orchestration: `autoPairPro`/`watchPro`/re-pin/hot-swap | **No** (no seam — see M4) |
| `mdns` register-once / no-goodbye invariant (the package's whole reason to exist) | **No** — enforced only by comment |
| `entertainment` Start/Stop/run interleaving | **No** — `Stop` doesn't join `run` (M2) |

---

## Recommended sequence

1. **H1** — route `cfg.Pro` reads through `GetPro()`, fix the in-place mutation in `watchPro`. *(Low effort, only
   finding that can misbehave at runtime.)*
2. **H2** — retire/flag the experimental scaffolding (profiles, MediaServer alias, entprobe, PUT trace). Biggest
   maintainability win; shrinks several packages.
3. **M3 + M4 + M5** — typed errors, a `Pairer` in `bridgepro`, and an `application` seam together unlock testing
   the resilience path and remove the triplication.
4. **H3 + M1 + M2** — fold the entertainment/DTLS state behind one lock and serialize streamer Start/Stop
   (add a `run` done-channel); these are coupled and best done together, then run
   `go test -race ./internal/entertainment/...` under a simulated fast-reconnect loop.
5. Low-severity cleanups (L1, L4, L5, L8) as opportunistic follow-ups.

*Note: `go vet` is clean across all packages. Findings above were derived from reading the code on this branch;
the runtime data race (H1) and the triplicated helpers/pairing sequence were verified directly.*
