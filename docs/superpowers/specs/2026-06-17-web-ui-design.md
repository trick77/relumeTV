# relume Web UI — Design

**Status:** approved design, ready for implementation planning
**Date:** 2026-06-17
**Scope:** an optional, opt-in web UI for relume: a guided setup assistant plus a
live status dashboard. Single-binary, no build step. UI copy is **English**.

## Goal

relume today is a headless daemon — its state (Bridge Pro pairing, TV pairings,
control mode, stream health, lights) is only visible in the log. This adds an
optional web UI that makes the **current state of setup** legible at a glance and
**guides a first-time user through the two pairings** (Bridge Pro link button, TV
Ambilight+Hue search) that are otherwise the most confusing part of getting
relume working.

The UI is **entirely optional** and **off by default**. It is activated by a
dedicated flag and never affects the TV-facing emulation or the Pro-facing client.

## Activation & flag

- New `serve` flag: **`-ui-port <port>`**.
  - Default `0` (and empty) → UI disabled. relume behaves exactly as today.
  - Any non-zero port → start the UI HTTP server on that port.
- Port 80 is reserved for the TV-facing emulated bridge and must not move, so the
  UI always runs on its own port. Recommended documented value: `33300`.
- The UI server is a separate `http.Server` from the clipv1 bridge server; it
  starts only when `-ui-port != 0` and shuts down on the same signal as `serve`.
- README/`docs/DESIGN.md`: document the flag, the recommended `8080`, and that
  with `network_mode: host` the chosen port is exposed on the LAN (see Security).
- `compose.yaml`: leave the UI off by default; show the `-ui-port 8080` line as a
  commented opt-in (`-ui-port 33300`) so users consciously enable it.

## Technical approach

- **Embedded, no build step.** Static HTML + CSS + vanilla JS, compiled into the
  binary via Go `embed`. No npm, no Node, no Vite — preserves relume's
  single-static-binary, single-language (Go) property and keeps CI unchanged.
- **Live updates via Server-Sent Events (SSE).** One `GET /api/events` SSE stream
  pushes state snapshots and incremental events (pairing arrived, stream up/down,
  light colour changed, log line). The page renders from snapshots and patches on
  events; no polling.
- New package **`internal/webui`**:
  - `server.go` — the `http.Server`, route wiring, `embed.FS` for assets.
  - `state.go` — an observable state model + an event hub (fan-out to SSE clients).
    This is the single source of truth the UI reads; the rest of relume *publishes*
    into it.
  - `assets/` — `index.html`, `app.css`, `app.js` (embedded).
- **Read-mostly with a few safe actions.** The UI subscribes to existing runtime
  signals; it does not reach into the TV/Pro paths directly. Actions it exposes
  (see below) call already-existing internal operations.

### How the UI gets its data (wiring)

relume already holds all the needed state; the UI must observe it without coupling
the core paths to the UI:

- `internal/config` — Bridge Pro pairing (host, name, bridgeId, cert pinned?),
  TV `ApiUsers` (paired clients, clientkey present?), identity → mDNS/SSDP name.
- `internal/bridge` / `internal/clipv1` — control mode, the "controlled light
  window" (which lights the TV is currently driving), idle-off state, the pending
  Bridge-Pro-link window and pending TV pairing.
- `internal/bridgepro` — live light list + current colour/on-off (proxied from Pro).
- `internal/entertainment` — stream up/down, REST-vs-DTLS active sink, frame rates.

**Mechanism:** introduce a small `webui.Publisher` interface (e.g.
`PublishEvent(Event)` / `SnapshotState() State`). The bridge wiring layer holds a
publisher that is a **no-op when the UI is disabled**, so the core has zero UI
dependency and zero overhead in the default headless mode. When `-ui-port` is set,
the real hub is injected. Components emit events they already log (pairing
accepted, stream start/stop, idle-off, Pro connected) — the UI hub mirrors a
bounded ring buffer of recent events for the live log.

## Screens

Two screens, same visual system, driven by the same state model. Which one shows
on `/` is decided by setup completeness: if Bridge Pro **and** at least one TV
client are paired → **dashboard**; otherwise → **setup assistant**. A user can
always navigate to the other.

### Visual language

Ambilight-themed: dark background with soft radial glows; light tiles render in
their **real live colours** with a matching glow. Accent colour `#7c8cff`. Status
semantics: green = ok, amber = waiting/fallback, red = error, grey/idle = off.
All copy in **English**. (Mockups discussed in German are translated on build;
canonical labels are listed per component below.)

### Screen 1 — Setup assistant (first run)

Vertical 3-step stepper, each step a card with state pill (`done` / `waiting` /
`open`):

1. **Pair Bridge Pro** — shows pinned-cert + app-key status once done; while
   pending, a pulsing "Press the link button on the Bridge Pro" prompt. Reflects
   the existing background auto-pair.
2. **Connect your TV** — instructions ("Start the Ambilight+Hue bridge search on
   your TV and pick relume"), the advertised name (`Philips Hue – XXXXXX`), and a
   live "Waiting for TV search…" indicator. Flips to green when the TV pairing
   arrives (relume auto-accepts, as today).
3. **Check lights & go** — after pairing, lists the Pro's lights; a **Test flash**
   action confirms relume controls them, then links to the dashboard.

### Screen 2 — Dashboard (steady state)

Top to bottom:

1. **Header** — wordmark, version, and a single **health pill** summarising the
   one-line status (e.g. "Active · streaming to Pro").
2. **Setup pipeline** — horizontal recap of the same four states:
   `Bridge Pro → TV pairing → Mode → Stream`, each with a tick/live dot and a
   one-line detail. This is the persistent "state of setup".
3. **Lights grid** (primary) — every Pro light as a tile in its live colour.
   TV-driven lights are outlined and marked "live"; others show on/off. Count
   header: "16 total · 7 driven by TV".
4. **Needs attention** card — only shown when something pends: a Bridge-Pro link
   window (pulsing) and/or a waiting TV pairing with an **Accept** button.
5. **Control mode** card — Entertainment vs REST-follow, which is active, plus
   live counters (fps to TV, send rate to Pro, idle-off timer, uptime).
6. **Live events** — colour-coded, human-readable tail of the recent event ring.

## Actions (safe, optional)

The UI is read-mostly. The only actions, each mapping to an existing internal op:

- **Accept TV pairing** — relume already auto-accepts; the button is a manual
  confirm for the rare case a user wants to gate it. (May be display-only in v1 if
  auto-accept stays unconditional — decided in the plan.)
- **Test flash** — reuse the existing flash routine on the controlled-light set.

No re-pair / mode-switch / per-light control in v1 (YAGNI; mode is a restart-time
flag and re-pairing is the `setup` command). These can come later.

## Endpoints

- `GET /` — the SPA (one embedded `index.html`).
- `GET /api/state` — current snapshot (JSON), so the page renders before the SSE
  stream warms up.
- `GET /api/events` — SSE stream of snapshots + incremental events.
- `POST /api/actions/accept-pairing` — accept a pending TV pairing (if exposed).
- `POST /api/actions/flash` — trigger the test flash.
- Static assets served from the embedded FS.

## Security

This is a hobby project on a trusted home LAN, and with `network_mode: host` the
UI port is reachable by anyone on that L2 network. v1 keeps it simple and
**documents the exposure** rather than adding auth:

- UI is **off by default**; the user opts in explicitly via `-ui-port`.
- The UI exposes light state and pairing status (no secrets: app keys, client
  keys, and the cert fingerprint are **never** sent to the browser).
- The two actions are low-risk (accept a pairing relume would auto-accept anyway;
  flash lights). They are not destructive and touch nothing outside relume's
  normal behaviour.
- README/`docs/DESIGN.md` note: "the UI has no authentication; only enable it on a
  trusted network." A bind-address / auth layer is explicitly out of scope for v1.

## Error handling

- UI server failing to bind (port in use) logs an error and **does not** take down
  `serve` — the bridge keeps running headless.
- SSE clients that disconnect are dropped from the hub; the ring buffer is bounded.
- If the Pro is unreachable, the lights grid shows a degraded state from the last
  snapshot with a "Pro unreachable" banner rather than blanking.
- The publisher being a no-op when disabled guarantees the headless path is
  unchanged and untested-by-the-UI.

## Testing

- `internal/webui` unit tests: state model transitions, event-hub fan-out,
  snapshot JSON shape, secret-redaction (assert app/client keys never serialise).
- SSE handler test: a subscriber receives snapshot then events; disconnect cleans
  up.
- A no-op-publisher test proving the core path emits nothing measurable when the
  UI is disabled.
- Manual/real-hardware check (per project norm): enable `-ui-port 8080`, walk the
  setup assistant on first run, confirm the dashboard reflects a live TV stream.
  Default documented port: `33300`.

## Out of scope (v1)

- Authentication / TLS / bind-address restriction for the UI.
- Re-pairing or mode switching from the UI.
- Per-light manual control or colour picking.
- Historical metrics / charts beyond the live event tail.
- A JS framework or any build step.

## Open items for the implementation plan

- Exact `webui.Publisher` interface and where it is injected in the bridge wiring.
- Whether **Accept pairing** is a real action or display-only in v1.
- The concrete `State` JSON schema (fields per component above).
