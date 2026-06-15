# AGENTS.md — relume

Module `github.com/trick77/relume`. Binary `relume`. Dir still named `ambibridge` (cosmetic).
Emulates a gen-2 Hue Bridge (BSB002) toward a Philips Ambilight TV; proxies to a real Hue
Bridge Pro (BSB003) via CLIP v2.

All repo content (docs, code comments, logs) is English.

## build/test
- `go build -o relume ./cmd/relume`
- `go test ./...`
- diagnostics: `relume serve -debug` (SSDP header log + mDNS observer + HTTP body log);
  `-disable-ssdp` runs mDNS-only (like ha-hue-entertainment) to isolate SSDP from discovery
- modes: `-mode rest` (DEFAULT, proven REST-follow — relume gives the generic stream-activation
  ack so the TV stays on per-light PUTs) or `-mode entertainment` (M4: confirms stream activation
  for real + runs the DTLS-PSK receiver on :2100, PSK = the clientkey relume minted for the TV at
  pairing). Entertainment is OPT-IN; REST is untouched. Confirmed 2026-06-15: the TV DOES use
  entertainment/DTLS once activation is confirmed (the old "REST-only" reading was circular).
- env diagnostics (no -debug flood): `RELUME_GAP_TRACE=1` logs inter-write gaps (idle-off
  calibration). `RELUME_ENT_PROBE=1` is the passive entertainment probe (REST mode only):
  confirms stream activation + watches udp :2100 for the TV's ClientHello + adds Hz to the
  `ambilight activity` rollup. Superseded by `-mode entertainment` (which services the stream).
  grep `ENTERTAINMENT` and `ambilight activity`. Probe never sends DTLS.
- commands: `serve` (default), `setup` (manual Pro pair — optional), `discover` (cloud), `avahi-service`, `version`
- pairing is auto-accepted (no link button, no UI) — but ONLY for the TV (source IP == `-tv-ip`, or
  the Android/Dalvik Philips-TV User-Agent); other LAN devices get error 101. POST /api is
  idempotent per devicetype (TV polls it fast). No web UI / no `link` command — everything via logs.
- backend Pro pairing is AUTOMATIC in `serve`: if no Pro is paired, a background goroutine
  (`autoPairPro`) discovers it (cloud, or `-bridge-ip`), pins the cert, and polls until the user
  taps the Pro's physical button (the only non-automatable step), then hot-loads lights. Runs
  independently of the TV side. `clipv1.Server` light provider is swapped at runtime (RWMutex).
  Once paired, `watchPro` health-checks the Pro every 60s and, on failure, re-discovers its
  IP / re-pins the cert / hot-swaps the provider — no re-pairing (appKey/clientKey persist).
- state lives in a Docker named volume `relume-data` (compose) at `/data/relume.json`.
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
- CONFIRMED root cause of "TV fetches description.xml but never lists/pairs relume": a powered-on
  real **Bridge Pro** on the same LAN (it also announces `_hue._tcp` as BSB003). Power the Pro OFF →
  the 65OLED806 instantly lists relume and sends `POST /api` (`devicetype=65OLED806/12`). Open
  product problem: relume must win over a powered-on Pro (the TV de-dupes/prefers BSB003) — NOT yet
  solved. (relume proxies control TO the Pro, so Pro-off only validates discovery/pairing.)
- mDNS announce MUST register exactly once and NEVER re-register/re-announce via
  `Server.Shutdown()`: grandcat/zeroconf's Shutdown multicasts an mDNS goodbye (TTL 0) that evicts
  relume from the TV's cache → bridge flickers out of the Ambilight list. This (not the descriptor)
  was the real discovery bug; fixed in `internal/mdns/announce.go`.
- Measured: the TV (65OLED806, Android 11) DOES actively query `_hue._tcp` during the Ambilight
  search (capture), then fetches plain `/description.xml`. It does NOT send hue SSDP M-SEARCH (only
  `MediaServer`), does NOT use cloud (no DNS for discovery.meethue.com).
- So mDNS announce is the primary path. Working ref = hass-emulated-hue: instance name exactly `Philips Hue - XXXXXX` (last 6 of bridgeid, spaces around dash), TXT bridgeid+modelid. diyHue name `DIYHue-XXXXXX` NOT found by TV.
- The real Bridge Pro also announces `_hue._tcp` as `Hue Bridge - XXXXXX` / `modelid=BSB003`. TV likely filters BSB003 out.
- Port 10102 broadcasts from the TV are DTS Play-Fi (audio), a red herring — not Hue.
- SSDP still served (3 ST: rootdevice, uuid, basic) but secondary. Respond instantly (short TV search window).
- multi-NIC: bind multicast to the interface owning advertise-IP, else Go uses the default iface (wrong LAN). Dual-homed host = bad test env. macOS system mDNSResponder owns 5353 → built-in announcer fails there; test on Linux (NAS).

## Bridge Pro (BSB003) facts
- HTTPS:443 only; HTTP:80 → 301. CLIP v2 only.
- cert self-signed Signify (CN=root-bridge, leaf OU=BSB003) → pin leaf SHA-256, do NOT trust CA chain. `-skip-tls-verify` fallback.
- pair = POST https://<ip>/api {devicetype,generateclientkey:true}; physical button = brief TAP not hold; error 101 = not pressed.
- PUT returns 207 multi-status with per-attribute `errors[]` even when HTTP-ok → inspect errors[], not just status code.
- CT-only lights reject `color.xy` → 207 error. v2 lights have no reliable id_v1 → assign stable v1 ids by sorted-UUID order.

## deployment
- needs same L2 as TV (SSDP+mDNS multicast) → Docker `network_mode: host`.
- rootless can't bind <1024. If TV hardcodes API port 80 (unconfirmed; SRV/LOCATION port may be honored instead), use host `sysctl net.ipv4.ip_unprivileged_port_start=80`, NOT a root container.
- CI: push/PR to master runs tests; push to master builds+pushes image to ghcr.io/trick77/relume (semver tag auto-bumped).

## toolchain trap
- go 1.26 + grandcat/zeroconf v1.0.0 pulls ancient golang.org/x/net that fails to link (`syscall.recvmsg`). Keep x/net, x/sys, x/crypto upgraded.

## secrets
- `relume.json` holds Pro appKey/clientkey + TV tokens. Gitignored. Never commit.

## status
M2 Pro client, M3 REST light control: done+verified on real Pro. M1 discovery+pairing: VERIFIED on
65OLED806 — TV lists relume and completes `POST /api` — but ONLY with the real Bridge Pro powered
OFF (coexistence is the open problem). Pairing is auto-accepted (TV-only) + idempotent; mDNS is
register-once (no goodbye); description.xml is text/xml. M4 entertainment (DTLS+HueStream) not
started. See PLAN.md.
