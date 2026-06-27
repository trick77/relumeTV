# relume-tv — Architecture Review (resolved)

Original review date: 2026-06-17 · Scope: full `internal/*` + `cmd/relume-tv` + build/CI.
Security was out of scope by request (LAN-only, no external access).

> **Status: backlog cleared.** All original findings have been remediated and verified
> against the code. The first 13 (H1–H3, M1, M2, M3, M6, L1, L3, L4, L8, L9, L10) landed
> in #67; the remaining seven (M4, M5, M7, L2, L5, L6, L7) in the follow-up cleanup. This
> file is kept as a record; reopen entries here if a regression reintroduces one.

## Verdict (unchanged)

A well-structured small Go codebase. Clean layering (no import cycles, `config` is a proper
leaf, `clipv1` uses textbook dependency inversion), hard-won protocol knowledge preserved as
deliberate invariants, and the trickiest mechanisms — the coalescing optimistic provider, the
REST/DTLS mutual exclusion, the sticky fallback watchdog — are sound and well-commented.

## How the final seven were resolved

- **M4 — `runServe` testability.** The Pro read+control interface is now `bridgepro.ProController`
  (producer-owned, with a compile-time assertion), aliased by `bridge.proClient`. `runServe`'s
  pure decision logic is extracted into `deriveServeConfig` (mode validation, ui/http port-clash,
  controlled-window sizing, summary cadence), unit-tested across every branch. The resilience
  helpers already had injectable seams (`proWatcher`) covered by the `tick` tests, so the
  goroutine orchestration was left as-is rather than risk a pure refactor of zero-coverage
  lifecycle code.
- **M5 — duplicated pairing.** The wait-for-button → set-keys → capture sequence is centralised
  in `bridgepro.Pairer.WaitForLinkButton`, parameterised by retry/report policy; `autoPairPro`
  and `runSetup` both call it. Unit-tested via injected seams.
- **M7 — `handleGroupAction`.** Group-action writes are now fanned out to every offered light
  through the same coalescing provider instead of being dropped; the `recordGroupActionWrite`
  tally remains as the tripwire. Covered by a test.
- **L2 — DTLS decode/forward.** `OnFrame` now flows through a small bounded queue drained by a
  forwarder goroutine; a full queue drops the newest frame (counted) so a slow sink degrades
  smoothly instead of stalling intake. Race-tested.
- **L5 — config durability.** Added `schemaVersion` (refuse newer), fsync of
  the temp file and directory on atomic save, and cleanup of orphaned `.tmp` files.
- **L6 — health check.** The liveness probe uses the lighter `BridgeInfo()` instead of `Lights()`.
- **L7 — defensive guards.** Short-id name fallback guarded; `toFloat` accepts int64/int32/float32;
  malformed xy components dropped; v2 HueStream channel ids >255 dropped rather than aliased.

## Residual test gaps (not findings — future hardening)

| Area | Tested? |
|---|---|
| `bridgepro/client.go` transport (get/post/put/del, **207 multi-status**, cert pin), `resources.go`, `discover.go` | **No** — exercised only indirectly |
| `runServe` goroutine wiring / shutdown ordering (the orchestration itself, vs the now-tested `deriveServeConfig` + `proWatcher.tick`) | **No** — would need integration-level listeners |
