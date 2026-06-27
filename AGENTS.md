# AGENTS.md — relume-tv

Module `github.com/trick77/relume-tv`. Binary `relume-tv` (`cmd/relume-tv`).
Emulates a gen-2 Hue Bridge (BSB002) toward a Philips Ambilight TV; proxies to a real Hue
Bridge Pro (BSB003) via CLIP v2.

All repo content (docs, code comments, logs) is English.

## build/test
- `go build -o relume-tv ./cmd/relume-tv`
- `go test ./...`
- diagnostics: `relume-tv serve -debug` (SSDP header log + mDNS observer + HTTP body log);
  `-disable-ssdp` runs mDNS-only (like ha-hue-entertainment) to isolate SSDP from discovery
- modes: `-mode entertainment` (DEFAULT; M4) or `-mode rest` (explicit fallback, proven REST-follow
  — relume-tv gives the generic stream-activation ack so the TV stays on per-light PUTs). Entertainment:
  confirms stream activation for real, runs the DTLS-PSK receiver on :2100 (PSK = the clientkey
  relume-tv minted for the TV) to decode the TV's HueStream, AND opens relume-tv's OWN entertainment
  stream TO the Pro over DTLS — creates/reuses a `relume-tv` entertainment_configuration, re-encodes
  frames as HueStream v2 at ~50Hz; PSK to the Pro = the Pro's appKey/clientkey. Auto-falls back to
  the per-light REST forward if DTLS can't establish (DTLS and REST are mutually exclusive — never
  both). The DTLS path EXISTS because per-light REST forwarding alone overflows the Pro's command
  queue (503). A relume-tv restart mid-stream orphans the TV's session: toggle Ambilight (not
  Ambilight+Hue) on the TV to reconnect.
- env diagnostics (no -debug flood): `RELUME_TV_GAP_TRACE=1` logs inter-write gaps (idle-off
  calibration). grep `ENTERTAINMENT` / `ambilight activity`.
- commands: `serve` (default), `avahi-service`, `version`
- pairing is auto-accepted (no link button, no UI) — but ONLY for the TV (source IP == `-tv-ip`, or
  the Android/Dalvik Philips-TV User-Agent); other LAN devices get error 101. POST /api is
  idempotent per devicetype (TV polls it fast). Pairing needs no `link` command and no UI step — it
  is driven via logs.
- web UI (setup assistant + live dashboard) is ON by default on :33100; `-headless` disables it,
  `-ui-port` moves it (must differ from -http-port). NO auth, so under
  `network_mode: host` it is LAN-reachable by anyone — read-only, never touches the control paths.
- backend Pro pairing is AUTOMATIC in `serve`, driven by the web-UI setup wizard: if no Pro is
  paired, a background goroutine (`autoPairPro`) discovers it via local mDNS (`_hue._tcp.local.`;
  picks the first real Hue Bridge Pro by advertised modelid BSB003, no cloud), pins the cert, and polls
  until the user taps the Pro's physical button (the only non-automatable step), then hot-loads
  lights. Runs independently of the TV side. `clipv1.Server` light provider is swapped at runtime
  (RWMutex). Once paired, `proWatcher` health-checks the Pro every 60s and, on failure, re-discovers
  its IP (cloud) / re-pins the cert / hot-swaps the provider — no re-pairing (appKey/clientKey persist).
- state lives in a Docker named volume `relume-tv-data` (compose) at `/data/relume-tv.json`.
- container build file is `Containerfile` (not Dockerfile); compose file is `compose.yaml`

## identity invariants (TV rejects otherwise)
- `modelid` MUST be `BSB002` in /config, mDNS TXT, SSDP. In description.xml this is the
  `<modelNumber>`: real bridges send `929000226503`, but the confirmed-working
  ha-hue-entertainment emulator sends `BSB002` — so `BSB002` is fine; do not change it blindly.
- description.xml MUST be served as `Content-Type: text/xml` (not application/xml); real bridges
  and ha-hue-entertainment use text/xml.
- bridgeid = upper(serial[:6] + "FFFE" + serial[6:]); serial = 12 hex; UUID = `2f402f80-da50-11e1-9b23-<serial>`.
- UUID identical across SSDP USN, description.xml UDN. bridgeid identical across SSDP hue-bridgeid header, mDNS TXT, /config.

## discovery (the hard part)
- CONFIRMED root cause of "TV fetches description.xml but never lists/pairs relume-tv": a powered-on
  real **Bridge Pro** on the same LAN (it also announces `_hue._tcp` as BSB003). Power the Pro OFF →
  the 65OLED806 instantly lists relume-tv and sends `POST /api` (`devicetype=65OLED806/12`). Open
  product problem: relume-tv must win over a powered-on Pro (the TV de-dupes/prefers BSB003) — NOT yet
  solved. (relume-tv proxies control TO the Pro, so Pro-off only validates discovery/pairing.)
- mDNS announce MUST register exactly once and NEVER re-register/re-announce via
  `Server.Shutdown()`: grandcat/zeroconf's Shutdown multicasts an mDNS goodbye (TTL 0) that evicts
  relume-tv from the TV's cache → bridge flickers out of the Ambilight list. This (not the descriptor)
  was the real discovery bug; fixed in `internal/mdns/announce.go`.
- Measured (65OLED806/Android 11): the TV actively queries `_hue._tcp` then fetches plain
  `/description.xml`; NO hue SSDP M-SEARCH (only `MediaServer`), NO cloud. So mDNS announce is the
  PRIMARY path. Working ref = hass-emulated-hue: instance name exactly `Philips Hue - XXXXXX` (last 6
  of bridgeid, spaces around the dash), TXT bridgeid+modelid. diyHue `DIYHue-XXXXXX` is NOT found.
- The real Bridge Pro announces `_hue._tcp` as `Hue Bridge - XXXXXX`/`modelid=BSB003`; TV likely
  filters BSB003 out. Port 10102 TV broadcasts = DTS Play-Fi (audio), red herring.
- SSDP still served (3 ST: rootdevice, uuid, basic) but secondary; respond instantly (short window).
- multi-NIC: bind multicast to the iface owning advertise-IP (else Go picks the default iface = wrong
  LAN). Dual-homed host = bad test env. macOS mDNSResponder owns 5353 → test on Linux (NAS).

## Bridge Pro (BSB003) facts
- HTTPS:443 only; HTTP:80 → 301. CLIP v2 only.
- cert self-signed Signify (CN=root-bridge, leaf OU=BSB003) → pin leaf SHA-256, do NOT trust CA chain. `-skip-tls-verify` fallback.
- pair = POST https://<ip>/api {devicetype,generateclientkey:true}; physical button = brief TAP not hold; error 101 = not pressed.
- PUT returns 207 multi-status with per-attribute `errors[]` even when HTTP-ok → inspect errors[], not just status code.
- CT-only lights reject `color.xy` → 207 error. So `translate.LightsV1` offers ONLY color-capable
  lights (CLIP v2 `color` present); white/CT/dimmable/on-off are filtered out. Light type/modelid
  reflect real capability (not always "Extended color light"). v2 lights have no reliable id_v1 →
  assign stable v1 ids by sorted-UUID order over the KEPT lights.
- `translate.StateV1ToV2` must accept `xy` as `[]any` (TV JSON REST path) AND `[]float64`/`[]float32`
  (the in-process entertainment decode path, `entertainment.ToHueV1State`). A type-assert to only
  `[]any` silently drops the colour → lights stuck on their last colour (the "stuck red" bug).

## deployment
- needs same L2 as TV (SSDP+mDNS multicast) → Docker `network_mode: host`.
- rootless can't bind <1024. If TV hardcodes API port 80 (unconfirmed; SRV/LOCATION port may be honored instead), use host `sysctl net.ipv4.ip_unprivileged_port_start=80`, NOT a root container.
- CI: push/PR to master runs tests; push to master builds+pushes image to ghcr.io/trick77/relume-tv (semver tag auto-bumped).

## toolchain trap
- go 1.26 + grandcat/zeroconf v1.0.0 pulls ancient golang.org/x/net that fails to link (`syscall.recvmsg`). Keep x/net, x/sys, x/crypto upgraded.

## secrets
- `relume-tv.json` holds Pro appKey/clientkey + TV tokens. Gitignored. Never commit.

## status
M1 discovery+pairing: VERIFIED on 65OLED806 — TV lists relume-tv and completes `POST /api` — but ONLY
with the real Bridge Pro powered OFF (coexistence is the open problem). M2 Pro client, M3 REST light
control: done+verified on real Pro. M4 entertainment (DTLS+HueStream): Phase A–C done and VERIFIED on
the real TV+Pro (2026-06-16) — relume-tv decodes the TV stream and streams it on to the Pro over its own
DTLS entertainment_configuration; the 503 command-queue overflow is gone. Phase D (config persistence
+ activation lifecycle) is next. Pairing is auto-accepted (TV-only) + idempotent; mDNS is
register-once (no goodbye); description.xml is text/xml.
