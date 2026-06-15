# relume — plan & status

Software bridge connecting a **Philips Ambilight TV** to a **Hue Bridge Pro (BSB003)** by
emulating an old gen-2 bridge (BSB002) toward the TV and proxying everything to the real
Bridge Pro over HTTPS/CLIP v2.

```
Ambilight TV  ──mDNS/SSDP + HTTP──▶  relume  ──HTTPS/CLIP v2──▶  Hue Bridge Pro  ──Zigbee──▶  lights
```

## Why

The Bridge Pro breaks the Ambilight+Hue path in three ways:
1. **No SSDP/UPnP** — only mDNS + cloud; the TV firmware expects to discover via the local bridge.
2. **HTTPS:443 only** — no plain HTTP:80 (returns 301); the TV firmware is wired for HTTP.
3. **CLIP v2 only** — the v1 discovery/pairing paths the TV uses no longer resolve.

## Decisions

| Topic | Decision |
|------|----------|
| Base | Standalone Go proxy (diyHue is reference only, not a fork) |
| Language | Go |
| Deployment | Docker with `network_mode: host` (multicast discovery needs the TV's L2) |
| Lights | Proxied live from the Bridge Pro |
| Path | Full: Entertainment + REST fallback |
| Bridge Pro setup | One-time pairing; pin the TLS certificate (default), `-skip-tls-verify` fallback |
| File naming | `Containerfile`, `compose.yaml`; CI workflows `.yaml` |

## Architecture

**Frontend (TV-facing, emulates BSB002):**
- `internal/ssdp` — multicast responder (M-SEARCH) + periodic NOTIFY ssdp:alive.
- `internal/mdns` — active `_hue._tcp` announcer (`Philips Hue - XXXXXX`, TXT bridgeid+modelid=BSB002).
- `internal/upnp` — `/description.xml` with the BSB002 identity.
- `internal/clipv1` — HTTP server: pairing (`POST /api`, link window), `config`, lights/groups, REST control.

**Backend (Bridge Pro-facing, acts as a Hue app):**
- `internal/bridgepro` — CLIP v2 client (HTTPS + cert pinning), pairing, resource reads, REST control. *(Entertainment client: M4)*

**Core:**
- `internal/config` — persistent state: identity, TV tokens, Pro pairing, light mapping.
- `internal/translate` — v1↔v2 translation + v1-id↔UUID mapping.
- `internal/bridge` — wiring frontend↔backend.
- `internal/diag` — passive mDNS observer (debug).
- `cmd/relume` — `serve` (default), `setup`, `discover`, `link`, `avahi-service`, `version`.

## Milestones

- **M1 — Discovery & pairing** ⚠️ partial. The TV finds relume via mDNS and fetches
  `/description.xml`, but **does not pair**: no measured run has reached `POST /api`. NOT verified
  end-to-end. (See "Discovery finding" below; the earlier "done & verified" was inaccurate.)
- **M2 — Bridge Pro client** ✅ done & verified on the real BSB003. CLIP v2 client (HTTPS +
  cert pinning), `setup`/`discover`, v2→v1 light list (16 lights via the proxy).
- **M3 — REST control** ✅ light control done & verified (real lamp switched). `PUT lights/{id}/state`
  → v1→v2 → Pro (207/errors handled). Group path is still a logged stub (completed with M4).
- **mDNS discovery** ✅ implemented (active `_hue._tcp` announce) + `avahi-service` command.
  Final TV-detection test pending on Linux (see below).
- **M4 — Entertainment** 🚧 in progress (opt-in `-mode entertainment`; REST stays default).
  - Phase A ✅ `internal/huestream` parser (+tests) + `internal/entertainment` DTLS-PSK receiver
    on :2100 (PSK = the TV's minted clientkey), decodes + logs frames. Verified: TV uses DTLS.
  - Phase B ✅ forward decoded frames to the Pro via the coalescing REST provider
    (entertainment.ToHueV1State → clipv1.ForwardLight). Interim end-to-end: lights
    follow at the REST rate (~10 Hz/light); Phase C lifts this to true 25fps.
  - Phase C ⏳ DTLS client to the Pro (entertainment-config create/activate + HueStream encode)
    for true 25fps. Phase D ⏳ group persistence + activation lifecycle.
  Confirmed the TV opens a real DTLS entertainment stream once relume confirms activation.
- **M5 — Packaging** ✅ done. Containerfile (static, multi-stage), `compose.yaml` (host networking),
  README, CI (test + release to ghcr.io). Image builds.

## Discovery finding (measured on the real Philips TV)

- The TV sends no Hue-specific SSDP M-SEARCH and no cloud lookup for
  `discovery.meethue.com`.
- After TV reboot, the TV actively queries `_hue._tcp.local`, fetches plain
  `/description.xml` with the Android/Dalvik stack, later sends `MediaServer:1`
  SSDP M-SEARCH, and fetches `/description.xml?relume=ms1` with the Philips DLNA stack.
- No measured run has reached `POST /api`, `/api/config`, or authenticated `/api/...`.
  The current failure is after descriptor retrieval, not basic IP discovery.
- Diagnostics now support startup bursts: `-discovery-burst-duration 90s
  -discovery-burst-interval 1s` sends repeated SSDP NOTIFY and mDNS re-announcements while
  the TV is in Ambilight+Hue scan mode.
- `-debug -tv-ip <tv-ip>` logs every mDNS question from the TV, not only Hue-looking names.
  This separates active mDNS discovery from passive listening.
- `-identity-profile hass` switches the SSDP `SERVER` header and `description.xml`
  manufacturer fields to the Home Assistant emulated-hue shape. Public issue reports show
  Philips TVs accepting hass-emulated-hue even where diyHue discovery is unreliable.
- `-ssdp-media-server-alias` is an opt-in experiment for the measured Android TV behavior:
  it actively broadcasts a `MediaServer:1` SSDP NOTIFY and answers `MediaServer:1` M-SEARCH
  with cache-busted `LOCATION: /description.xml?relume=ms1` and `max-age=1`. Only that query
  URL serves `deviceType=MediaServer:1`; plain `/description.xml` remains Hue Basic for the mDNS path.
- `-ssdp-descriptor-variants` adds `LOCATION: /description.xml?relume=basic1` under the
  same `MediaServer:1` ST/NT. That URL still serves `deviceType=Basic:1`. It tests whether
  the TV follows the MediaServer SSDP trigger but rejects the MediaServer descriptor body.
- `-description-profile ambilight-reference` keeps the same discovery identity but changes
  `description.xml` formatting/friendlyName to match the active Ambilight OSS bridge more
  closely. It tests whether the TV parses then rejects relume's descriptor before starting
  CLIP v1 pairing.
- `-ssdp-media-server-basic-body` keeps the MediaServer SSDP trigger and the `?relume=ms1`
  URL that the TV actually follows, but serves a Hue Basic descriptor body from that URL.
  It tests whether the TV rejects the MediaServer descriptor type before starting CLIP v1 pairing.
- The real Bridge Pro itself announces `_hue._tcp` as `Hue Bridge - XXXXXX` / `modelid=BSB003`;
  the TV likely filters BSB003 out. relume announces `Philips Hue - XXXXXX` / `modelid=BSB002`.
- UDP 10102 broadcasts from the TV are DTS Play-Fi (audio) — a red herring, unrelated to Hue.
- macOS is an unusable test environment: the system mDNSResponder owns port 5353, so relume's
  built-in announcer cannot bind it. Final TV test belongs on single-homed Linux (the NAS).

### Discovery experiments already tried

| Version | Variation | Result |
| --- | --- | --- |
| `0.1.8` | Ambilight identity profile, Ambilight OSS-style `SERVER`, short CLIP v1 config, compatibility endpoints. | TV still stopped after descriptor discovery. |
| `0.1.9` | HTTP `Server`/`Cache-Control` on `description.xml`; MediaServer alias descriptor `max-age=1`. | No `/api` follow-up. |
| `0.1.10` | mDNS SRV host changed to lower bridgeid (`<bridgeid>.local.`). | TV HTTP `Host` stayed as the IP, so hostname multiplexing is not useful. |
| `0.1.11` | Ambilight serial, UDN, and SSDP UUID/USN changed to lower bridgeid with `FFFE`. | TV still stopped after descriptor fetch. |
| `0.1.12` | Basic:1 SSDP USN changed to `uuid::<urn:...:basic:1>`. | After TV reboot, it fetched plain `/description.xml` and `/description.xml?relume=ms1`; still no `/api`. |
| `0.1.13` | Added `-ssdp-descriptor-variants` and `/description.xml?relume=basic1`. | Windows Chromium/DIAL fetched `basic1`; the TV fetched plain `/description.xml` and `?relume=ms1` only. Still no `/api`. |
| `0.1.15` | Added `-description-profile ambilight-reference`. | TV fetched changed `?relume=ms1` descriptor bytes; still no `/api`. |
| `0.1.16` | Added `-ssdp-media-server-basic-body`. | Pending real-TV result. |
| `0.1.17` | `description.xml` served as `text/xml` (was `application/xml`). | Capture (65OLED806): TV queries `_hue._tcp`, fetches descriptor (200), then **nothing** — still not listed. Content-Type was NOT the cause. |
| next | mDNS register-once; removed Shutdown-based re-announce (was emitting goodbye/TTL-0 packets that evicted the bridge from the TV cache). | Root cause: relume's periodic re-announce flickered itself out of the TV's `_hue._tcp` cache; confirmed-working 83noit registers once. Awaiting real-TV retest. |

## Root cause (found after the table above)

relume's `description.xml`, mDNS records and `/config` are byte-equivalent to the confirmed-working
`83noit/ha-hue-entertainment` emulator (verified against its source; same TV series 55OLED806 vs the
user's 65OLED806). The difference was behavioral, not content: relume re-announced mDNS every 30s
(every 2s during the burst) via `grandcat/zeroconf` `Server.Shutdown()`, which multicasts an mDNS
goodbye (TTL 0). The Android TV actively queries `_hue._tcp`, caches the answer, then receives the
goodbye and drops the bridge. 83noit registers exactly once and never sends goodbye. Fix:
register-once in `internal/mdns/announce.go`.

## Open items (verify on the real device)

- **TV detection of relume on the Linux target** with debug burst plus tcpdump:
  `relume serve -debug -advertise-ip <nas-lan-ip> -tv-ip <tv-ip>
  -discovery-burst-duration 90s -discovery-burst-interval 1s` and
  `tcpdump -ni <iface> 'host <tv-ip> or udp port 5353 or udp port 1900 or tcp port 80'`.
  If the default identity is ignored, repeat with `-identity-profile ambilight`; if the TV
  fetches the MediaServer alias but still stops, add `-ssdp-descriptor-variants`.
- Exact `HueStream` v2 layout (52-byte header, channel chunks).
- Exact CLIP v2 calls to create/activate the `entertainment_configuration` on the Pro.
- Whether the TV requires a specific `swversion`/`apiversion` to attempt Entertainment.
- The exact `devicetype` string the TV sends to `POST /api`; whether the TV uses the
  mDNS-advertised port or hardcodes 80.

## Build / test

```bash
go build -o relume ./cmd/relume
go test ./...
go run ./cmd/relume serve -debug -http-port 8080 -advertise-ip <ip> -config ./relume.json
```
