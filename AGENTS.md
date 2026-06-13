# AGENTS.md — relume

Module `github.com/trick77/relume`. Binary `relume`. Dir still named `ambibridge` (cosmetic).
Emulates Hue Bridge gen2 (BSB002) to Philips Ambilight TV; proxies to real Hue Bridge Pro (BSB003) via CLIP v2.

## build/test
- `go build -o relume ./cmd/relume`
- `go test ./...`
- run diagnostics: `relume serve -debug` (SSDP header log + mDNS observer + HTTP body log)
- commands: `serve` (default), `setup` (pair Pro), `discover` (cloud), `link` (open 30s TV-pairing window)

## identity invariants (TV rejects otherwise)
- `modelid` MUST be `BSB002` everywhere: mDNS TXT, description.xml, /config.
- bridgeid = upper(serial[:6] + "FFFE" + serial[6:]); serial = 12 hex; UUID = `2f402f80-da50-11e1-9b23-<serial>`.
- UUID identical across SSDP USN, description.xml UDN. bridgeid identical across SSDP hue-bridgeid header, mDNS TXT, /config.

## discovery (the hard part)
- TV does NOT send SSDP M-SEARCH for hue in observed traffic. Primary path = mDNS `_hue._tcp` ANNOUNCE (TV listens passively, sends no query → observer seeing nothing ≠ TV ignoring mDNS).
- Working ref = hass-emulated-hue: instance name exactly `Philips Hue - XXXXXX` (last 6 of bridgeid, spaces around dash), port 443, TXT bridgeid+modelid. diyHue name `DIYHue-XXXXXX` NOT found by TV.
- Cloud-suppression (diyHue #988): if a real bridge is registered at discovery.meethue.com, TV resolves it via cloud and skips local discovery entirely. `relume discover` here returns the real Pro → suspect this. Cloud N-UPnP is account-independent.
- SSDP still served (3 ST: rootdevice, uuid, basic) but secondary. Respond instantly (short TV search window, #988).
- multi-NIC: bind multicast to interface owning advertise-IP, else Go uses default iface (wrong LAN). Dual-homed host = bad test env.

## Bridge Pro (BSB003) facts
- HTTPS:443 only; HTTP:80 → 301. CLIP v2 only.
- cert self-signed Signify (CN=root-bridge, leaf OU=BSB003) → pin leaf SHA-256, do NOT trust CA chain. `-skip-tls-verify` fallback.
- pair = POST https://<ip>/api {devicetype,generateclientkey:true}; physical button = brief TAP not hold; error 101 = not pressed.
- PUT returns 207 multi-status with per-attribute `errors[]` even when HTTP-ok → inspect errors[], not just status code.
- CT-only lights reject `color.xy` → 207 error. v2 lights have no reliable id_v1 → assign stable v1 ids by sorted-UUID order.

## deployment
- needs same L2 as TV (SSDP+mDNS multicast) → Docker `--network=host`.
- rootless can't bind <1024. If TV hardcodes API port 80 (unconfirmed; SRV/LOCATION port may be honored instead), use host `sysctl net.ipv4.ip_unprivileged_port_start=80`, NOT root container.

## toolchain trap
- go 1.26 + grandcat/zeroconf v1.0.0 pulls ancient golang.org/x/net that fails to link (`syscall.recvmsg`). Keep x/net, x/sys, x/crypto upgraded.

## secrets
- `relume.json` holds Pro appKey/clientkey + TV tokens. Gitignored. Never commit.

## status
M1 discovery/pairing, M2 Pro client, M3 REST light control: done+verified on real Pro. M4 entertainment (DTLS+HueStream) not started. See PLAN.md.
