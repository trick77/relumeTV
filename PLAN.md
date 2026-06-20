# relumeTV — plan & status

Software bridge connecting a **Philips Ambilight TV** to a **Hue Bridge Pro (BSB003)** by
emulating an old gen-2 bridge (BSB002) toward the TV and proxying everything to the real
Bridge Pro over HTTPS/CLIP v2.

```
Ambilight TV  ──mDNS/SSDP + HTTP──▶  relumeTV  ──HTTPS/CLIP v2──▶  Hue Bridge Pro  ──Zigbee──▶  lights
```

> Architecture, control modes and identity invariants live in `docs/DESIGN.md`.
> Operational issues and the full discovery experiment history live in `docs/TROUBLESHOOTING.md`.

## Status

Done & verified on real hardware: **M2** Bridge Pro client (CLIP v2 + cert pinning),
**M3** REST control, **mDNS** active `_hue._tcp` announce, **M4** Entertainment
(Phase A receiver → B REST forward → C own DTLS stream to the Pro → C.1 watchdog
fallback → D group persistence), **M5** packaging. `-mode entertainment` is the default
with REST as the automatic fallback. The optional web UI (`-ui-port`) is shipped.

The one milestone **not** verified end-to-end is **M1 — discovery & pairing**: the TV
finds relumeTV via mDNS and fetches `/description.xml`, but no measured run has reached
`POST /api`. This is the active open problem (see below).

## M1 — discovery finding (measured on the real Philips TV)

- The TV sends no Hue-specific SSDP M-SEARCH and no cloud lookup for `discovery.meethue.com`.
- After TV reboot, the TV actively queries `_hue._tcp.local`, fetches plain
  `/description.xml` with the Android/Dalvik stack, later sends `MediaServer:1`
  SSDP M-SEARCH, and fetches `/description.xml?relumetv=ms1` with the Philips DLNA stack.
- No measured run has reached `POST /api`, `/api/config`, or authenticated `/api/...`.
  The failure is after descriptor retrieval, not basic IP discovery.
- The real Bridge Pro itself announces `_hue._tcp` as `Hue Bridge - XXXXXX` /
  `modelid=BSB003`; the TV likely filters BSB003 out. relumeTV announces
  `Philips Hue - XXXXXX` / `modelid=BSB002`. **Coexistence with a powered-on Pro on the
  same LAN is the open product problem** — see `docs/TROUBLESHOOTING.md`.
- macOS is an unusable test environment: the system mDNSResponder owns port 5353, so
  relumeTV's announcer cannot bind it. Final TV test belongs on single-homed Linux (the NAS).

### Root cause (found and fixed)

relumeTV's `description.xml`, mDNS records and `/config` are byte-equivalent to the
confirmed-working `83noit/ha-hue-entertainment` emulator. The difference was behavioral,
not content: relumeTV re-announced mDNS every 30s via `grandcat/zeroconf`
`Server.Shutdown()`, which multicasts an mDNS goodbye (TTL 0); the Android TV caches the
`_hue._tcp` answer, then receives the goodbye and drops the bridge. 83noit registers
exactly once and never sends goodbye. Fix: register-once in `internal/mdns/announce.go`.
Awaiting real-TV retest.

## Open / next steps

1. **Verify M1 discovery on the Linux target** with debug burst plus tcpdump:
   `relumetv serve -debug -advertise-ip <nas-lan-ip> -tv-ip <tv-ip>
   -discovery-burst-duration 90s -discovery-burst-interval 1s` and
   `tcpdump -ni <iface> 'host <tv-ip> or udp port 5353 or udp port 1900 or tcp port 80'`.
2. **Verify the TV-side watchdog fallback (Phase C.1) on real hardware.** The state machine
   is unit-tested, but the load-bearing unknown is: once relumeTV reverts to inactive, does
   the TV actually resume per-light PUTs? Ideally test a TV that does NOT open the DTLS
   stream. Reserve: an active stream-stop nudge (Ansatz 2 from the watchdog design).
3. **Colour accuracy on the Pro path — hardware-only eyeball check.** Decided: no code
   change. The DTLS path forwards the TV's HueStream frame verbatim (the `dtlsLoopback`
   test asserts pass-through); only the REST fallback does XY↔RGB in `ToHueV1State`.

### Other open unknowns (verify on the real device)

- Exact `HueStream` v2 layout (52-byte header, channel chunks).
- Whether the TV requires a specific `swversion`/`apiversion` to attempt Entertainment.
- The exact `devicetype` string the TV sends to `POST /api`; whether the TV uses the
  mDNS-advertised port or hardcodes 80.

## Build / test

```bash
go build -o relumetv ./cmd/relumetv
go test ./...
go run ./cmd/relumetv serve -debug -http-port 8080 -advertise-ip <ip> -config ./relumetv.json
```
