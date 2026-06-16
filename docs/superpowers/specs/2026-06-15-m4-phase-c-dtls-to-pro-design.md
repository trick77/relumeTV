# M4 Phase C — relume streams to the Bridge Pro over DTLS

## Context / Problem

M4 Phase B was verified on the real TV + Pro (2026-06-15): in `-mode entertainment`
the TV streams HueStream v1/XY at ~24.6 fps over DTLS:2100, relume decodes each
frame and forwards every channel to the Pro via the coalescing REST provider, and
the lights follow. **But the REST path saturates the Pro:** the log shows
`forwarding lights to bridge pro failing ... status 503: command queue is full,
please try later` with `coalesced_frames` accumulating. Per-light CLIP v2 PUTs
cannot sustain the stream — the Pro rate-limits writes (~10/s per light) and the
command queue overflows.

Phase C removes the REST bottleneck: relume opens its **own** Entertainment stream
*to* the Pro (CLIP v2 `entertainment_configuration` + DTLS HueStream), the same
low-latency path a real Hue entertainment app uses. The decoded TV frames are
re-encoded and streamed to the Pro at full rate instead of being turned into
per-light REST writes.

## Goal

In `-mode entertainment`, when the TV activates its stream, relume:
1. ensures a relume-owned `entertainment_configuration` exists on the Pro (covering
   the color-capable lights) and starts it,
2. opens a DTLS-PSK client to the Pro on udp:2100,
3. re-encodes each decoded TV frame as a HueStream v2 frame and streams it to the
   Pro at a steady rate,
4. falls back to the existing REST forward (Phase B) if Phase C cannot establish,
5. logs every step clearly so an operator can tell which path is active.

REST mode (`-mode rest`, the default) is untouched.

## Non-goals

- No change to TV-facing discovery/pairing or REST mode.
- No Hue-app interaction; relume creates its own entertainment configuration.
- No spatial/positional accuracy in the Pro config — positions are cosmetic for
  pass-through streaming and may be assigned trivially.

## Decisions (approved)

- **1A — relume owns the config.** relume looks for its own
  `entertainment_configuration` (metadata name `relume`); if none exists it creates
  one whose channels map to all color-capable lights. `start`/`stop` per TV stream.
  Chosen over reusing a Hue-app area: works with no manual setup and gives relume a
  deterministic channel→light mapping.
- **2A — auto-fallback to REST.** If config creation, `start`, or the DTLS handshake
  fails, relume falls back to the Phase B REST forward so the lights still follow
  (capped). DTLS and REST forwarding are mutually exclusive at runtime — never both.

## Architecture

```
TV ──DTLS v1/xy──▶ entertainment.Receiver (Phase A: decode)
                          │ OnFrame(frame)
                          ▼
                   ProStreamer  ──(DTLS up?)──▶ huestream.Encode(v2) ──DTLS──▶ Pro:2100
                          │                          (Phase C)
                          └──(DTLS down)────────────▶ clip.ForwardLight (REST, Phase B fallback)
```

### Components (new / extended)

1. **`internal/huestream` — `Encode(*Frame) []byte`** (new, pure, table-tested):
   inverse of the existing `Parse`. Emits the v2 wire format: 16-byte header
   (`"HueStream"` + `0x02 0x00` + sequence + 2 reserved + colorspace + 1 reserved),
   then 36-byte config-id (ASCII UUID), then 7 bytes/channel (1-byte channel id +
   3×uint16 big-endian). Round-trips with `Parse` in tests.

2. **`internal/bridgepro` — entertainment config calls** (new methods on `Client`):
   - `EntertainmentServices() ([]EntertainmentService, error)` — GET
     `/clip/v2/resource/entertainment`; each carries its `owner` device rid and
     segment info, used to map a light → its entertainment service rid.
   - `CreateEntertainmentConfig(name string, members []ConfigMember) (string, error)`
     — POST `/clip/v2/resource/entertainment_configuration`; returns the new UUID.
     Builds `configuration_type: "screen"`, `metadata.name`, `locations.service_locations`
     (per-light entertainment service rid + a trivial position) and `channels`
     (one channel per light, `channel_id` 0..N-1, `members` referencing the
     service). The exact nested payload is validated against the real Pro during
     implementation (see Open items).
   - `StartStream(id)` / `StopStream(id)` — PUT
     `/clip/v2/resource/entertainment_configuration/{id}` with `{"action":"start"}`
     / `{"action":"stop"}`.
   - Reuses the existing `EntertainmentConfigs()` (GET list) to find an existing
     `relume` config before creating one.

3. **`internal/entertainment` — DTLS client + ProStreamer** (new, mirrors the
   receiver):
   - DTLS-PSK **client** via `pion/dtls/v3` `dtls.Dial` to `<pro-host>:2100`,
     PSK identity = the Pro `appKey` (username), PSK = `clientKey` (hex→bytes),
     cipher `TLS_PSK_WITH_AES_128_GCM_SHA256`, `DisableExtendedMasterSecret` —
     same options as the receiver, just dialing instead of listening.
   - `ProStreamer`: owns the lifecycle. On TV stream activation: ensure+start config,
     dial DTLS, then run a send loop. `Push(frame)` from the receiver's `OnFrame`
     updates the latest per-channel colors; a ticker (~25–50 Hz) encodes and sends
     the current frame (re-sending the last frame when idle so the Pro doesn't stop
     the area after a few seconds). On TV stream deactivation / ctx cancel: stop the
     config, close DTLS.
   - **Channel remap:** the TV frame's channel id is a v1 light id; relume maps it to
     the Pro config's `channel_id` via a `map[uint16]uint8` built when the config is
     created (v1 id → assigned Pro channel). Color values (xy + bri) pass through.

4. **`cmd/relume/main.go` wiring:** in entertainment mode, construct the ProStreamer
   (needs the `bridgepro.Client`, the v1→UUID provider, the Pro `appKey`/`clientKey`,
   the bind/host IP, and `clip.ForwardLight` as the fallback sink). Replace the
   current `OnFrame` REST loop with `streamer.Push`; the streamer decides DTLS vs
   REST fallback internally so `main` stays simple.

## Logging (hard requirement — must be unambiguous)

Every Phase C lifecycle transition logs a distinct line, and the active path is
always identifiable:

- Config: `pro entertainment config ready id=<uuid> name=relume reused=true|false channels=<n>`
- Start/stop: `pro entertainment stream started id=<uuid>` / `... stopped id=<uuid>`
- DTLS: `pro DTLS stream connecting host=<ip>:2100 identity=<appKey>` then
  `pro DTLS stream connected` (or `pro DTLS handshake failed err=...`).
- **Active path** in the periodic `ambilight activity` rollup gains a field
  `forward_path=dtls|rest` so a single line shows whether Phase C is live or it fell
  back. The Phase-C send rate is logged in a periodic `pro entertainment stream`
  rollup (`frames_5s`, `channels`, `seq`).
- Fallback: `pro entertainment unavailable, falling back to REST forward err=...`
  at WARN, logged once per transition (not per frame).
- Recovery: if DTLS re-establishes, `pro DTLS stream connected (recovered), leaving REST fallback`.

The intent: from `docker logs relume | grep -E 'pro entertainment|pro DTLS|forward_path'`
the operator sees exactly which phase/path is running.

## Error handling

- Config create / start / DTLS dial failures → log once, set path=rest, keep serving
  via `ForwardLight`. Retry establishing Phase C on a backoff while the TV stream
  stays active.
- DTLS send error mid-stream → close, switch to REST fallback, attempt re-dial.
- On shutdown / TV deactivation → best-effort `StopStream`; never leave the Pro area
  active (it would block other entertainment apps).
- The Pro `clientKey` may be absent on older pairings → cannot DTLS → permanent REST
  fallback with a clear one-time WARN telling the user to re-pair for Phase C.

## Testing

- `internal/huestream`: `Encode` unit tests + `Parse(Encode(f)) == f` round-trip for
  v1 and v2 frames, including the 36-byte config-id and multi-channel payloads.
- `internal/bridgepro`: table tests for the create-config payload shape and the
  start/stop bodies against a stub HTTP server (mirroring existing client tests).
- `internal/entertainment`: ProStreamer state-machine test — Push without DTLS routes
  to the REST sink; with DTLS up routes to the encoder; fallback/recovery transitions
  log once and flip `forward_path`. A loopback DTLS test (client dials the existing
  receiver) verifies handshake + a decoded round-trip frame.
- Manual end-to-end on the NAS (`-mode entertainment`): confirm
  `pro DTLS stream connected`, `forward_path=dtls`, **no** more `503 command queue
  is full`, and smoother lights than Phase B.

## Open items (verify against the real Pro during implementation)

- Exact `entertainment_configuration` POST payload accepted by BSB003: the precise
  `channels`/`locations.service_locations` nesting, required `position` ranges, and
  whether `configuration_type` must be `screen`. Validate by reading one existing
  config first (GET) and mirroring its shape.
- Whether the Pro requires the channels' `members` to reference an entertainment
  service *segment* index vs the service rid directly.
- Confirm the Pro accepts HueStream **v2** frames (vs v1) and the steady-rate /
  keepalive interval that keeps the area from auto-stopping.

## Rollout

Feature branch `feat/m4-phaseC-dtls-to-pro` → PR → merge. Default `-mode rest`
unchanged; Phase C only affects opt-in `-mode entertainment`.
