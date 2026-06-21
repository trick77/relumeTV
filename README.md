# relume-tv

> **⚠️ Disclaimer — read this first**
>
> This is an experimental hobby project, built for fun and scratching my own itch.
> It works on *my* machine, on *my* network, with *my* hardware — and that's all I
> can vouch for. I can't help debug why it doesn't work for you, and I make no
> promises that it ever will. Running an old gen-2 bridge and a new Bridge Pro
> side by side is finicky by Philips' own design, so expect rough edges. Use it at
> your own risk; no support, no warranty, no guarantees.

## The problem

A Philips Ambilight TV's built-in Ambilight+Hue feature mirrors the on-screen colours onto your
Hue lights by pairing with the bridge. The new Hue **Bridge Pro (BSB003)** dropped the discovery
paths and v1 HTTP API surface that feature expects, so the TV can no longer find or pair with it
directly — and because Philips Hue (Signify) and the TV division (TP Vision) don't coordinate,
there is no official fix.

Developed and tested against a **Philips 65OLED806** (Android TV 11). The rest of the
**OLED806 series (48/55/65/77")** shares the same platform and firmware and should behave the
same; other pre-2023 Ambilight models are untested but may work. Note that 2025/2026 TVs ship
the new **AmbiScape** feature, which talks to bulbs directly over Matter and bypasses the Hue
Bridge entirely — those TVs neither need nor work with relume-tv.

## What this does

relume-tv sits between the TV and the Bridge Pro: to the TV it impersonates an old gen-2 bridge
(BSB002) speaking the v1 HTTP API, and it proxies every request to the real Bridge Pro over
HTTPS/CLIP v2. See [docs/DESIGN.md](docs/DESIGN.md) for identity, pairing, and the two control
modes.

## Requirements

- **A Philips Ambilight TV** with the built-in **Ambilight+Hue** feature (the older
  integration, roughly pre-2023 models) — the TV is what discovers and drives the bridge.
- **A Hue Bridge Pro (BSB003)**, already set up with your lights and reachable on the LAN.
- **A Linux host** to run relume-tv on. Discovery uses multicast (mDNS/SSDP), so the container
  needs `network_mode: host` — **Docker Desktop on macOS/Windows won't work**, its
  host-networking mode doesn't reliably carry the multicast traffic.
- **TV, relume-tv host, and Bridge Pro on the same L2 network / VLAN**, with multicast allowed
  (no client/AP isolation between them). For the lowest latency, connect all three over wired
  Ethernet — Wi-Fi can add noticeable lag to the Ambilight follow.
- **TCP port 80 free on the host** — relume-tv emulates a gen-2 bridge, which the TV reaches on
  `:80`. The TV **hardcodes** this port and ignores any port advertised over mDNS/SSDP, so `:80`
  is effectively mandatory — don't move it. Under rootless Docker, binding `:80` also requires
  `sysctl net.ipv4.ip_unprivileged_port_start=80` (or run the container with the privilege to
  bind low ports).

## Quick start (Docker)

1. Start the service:

   ```bash
   docker compose up -d
   ```

2. Open the web UI at `http://<host>:33100` and follow the guided setup wizard. It walks
   through pairing the Bridge Pro (a brief **TAP** of its link button when prompted — do not
   hold it), rebooting the TV, the TV's Ambilight+Hue scan, and assigning the bulbs.

### Data & secrets

State (bridge identity, TV tokens, **Bridge Pro app key + client key**) lives in
`./data/relume-tv.json`. This file holds secrets.

## Usage

### Commands

| Command | Purpose |
|---------|---------|
| `serve` | Run the service (discovery + bridge emulation). Default. |
| `avahi-service` | Emit an Avahi service file (for hosts where a system Avahi daemon owns mDNS). |
| `version` | Print the version. |

### Flags (`serve`)

- **`-mode`** &nbsp;·&nbsp; default `entertainment` — Control mode: `entertainment` (low-latency DTLS stream to the Pro; auto-falls back to REST if the TV never opens its stream) or `rest` (per-light REST-follow). See [docs/DESIGN.md](docs/DESIGN.md#control-modes).
- **`-advertise-ip`** &nbsp;·&nbsp; default auto — IP advertised via mDNS/SSDP; set it on a multi-homed host.
- **`-idle-off-timeout-rest`** &nbsp;·&nbsp; default `30s` — Rest mode: when the TV stops driving the lights for this long, turn them off (the TV sends no off signal, it just goes silent). REST writes are sparse and pause on static scenes, so this is longer than the entertainment timeout. `0` disables.
- **`-idle-off-timeout-entertainment`** &nbsp;·&nbsp; default `5s` — Entertainment mode: same as above but for the DTLS stream path (~50 Hz, stops cleanly when the TV goes off), so a short timeout is safe. `0` disables.
- **`-entertainment-dtls-timeout`** &nbsp;·&nbsp; default `5s` — Entertainment mode: how long to wait, after confirming the TV's stream activation, for the TV to open its DTLS stream before reverting to REST-follow. Raise it if a TV opens its stream slower.
- **`-entertainment-fallback-recovery`** &nbsp;·&nbsp; default `90s` — Entertainment mode: how long a latched REST fallback persists before the next TV stream activation may recover it (so a transient DTLS failure no longer pins the TV to REST until restart). Set `0` to disable (fallback stays sticky until restart).
- **`-entertainment-smooth-tau`** &nbsp;·&nbsp; default `40ms` — Entertainment mode: exponential-smoothing time constant for easing the TV's hard scene cuts on the DTLS send path. Lower is snappier but flickers more, higher is smoother but laggier. Set `0` to disable smoothing (frames forwarded verbatim).
- **`-skip-tls-verify`** &nbsp;·&nbsp; default off — Skip Bridge Pro certificate pinning (fallback).
- **`-headless`** &nbsp;·&nbsp; default off — Disable the web UI. The web UI (setup assistant + live status dashboard) is **on by default** on the predefined port `33100`. ⚠️ **No authentication: with `network_mode: host` the dashboard is reachable by anyone on the LAN out of the box.** Pass `-headless` to turn it off on untrusted networks. (`-ui` is still accepted as a no-op for backward compatibility.)
- **`-ui-port`** &nbsp;·&nbsp; default `33100` — Web UI port. Pass `0` to use the predefined port `33100`, or any other value to override it. Must differ from the HTTP port (80). Ignored when `-headless` is set.
- **`-debug`** &nbsp;·&nbsp; default off — SSDP/HTTP diagnostics + mDNS observer.

## Development

```bash
go build -o relume-tv ./cmd/relume-tv
go test ./...
```

## Documentation

- **[docs/DESIGN.md](docs/DESIGN.md)** — how relume-tv works: identity, pairing, control modes.
