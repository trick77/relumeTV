<p align="center">
  <img src="docs/relume.png" alt="relume" width="140">
</p>

# relume

A software bridge that connects a **Philips Ambilight TV** to a **Hue Bridge Pro (BSB003)**.
relume presents itself to the TV as an old gen-2 bridge (BSB002) and proxies every request
to the real Bridge Pro over HTTPS/CLIP v2.

![How relume sits between the Ambilight TV and the Hue Bridge Pro](docs/architecture.png)

> **⚠️ Disclaimer — read this first**
>
> This is an experimental hobby project, built for fun and scratching my own itch.
> It works on *my* machine, on *my* network, with *my* hardware — and that's all I
> can vouch for. I can't help debug why it doesn't work for you, and I make no
> promises that it ever will. Running an old gen-2 bridge and a new Bridge Pro
> side by side is finicky by Philips' own design, so expect rough edges. Use it at
> your own risk; no support, no warranty, no guarantees.

How it works (identity, pairing, the two control modes): see [docs/DESIGN.md](docs/DESIGN.md).

## Requirements

- relume must run on the **same L2 network** as the TV (discovery uses multicast).
  → Docker requires `network_mode: host`.
- A reachable Hue Bridge Pro on the same network.

## Quick start (Docker)

```bash
# 1. Pair with the real Bridge Pro (once). When prompted, briefly TAP the link
#    button on the Bridge Pro (do not hold it). Add -bridge-ip <ip> if cloud
#    discovery finds nothing.
docker compose run --rm relume setup -config /data/relume.json

# 2. Start the service
docker compose up -d

# 3. On the TV, start the Ambilight+Hue bridge search and select relume.
#    relume auto-accepts the TV's pairing — no button to press on relume's side.
```

The image is pulled from `ghcr.io/trick77/relume` (built by the release workflow).
To build locally instead: `docker build -f Containerfile -t relume:dev .`

## Commands

| Command | Purpose |
|---------|---------|
| `serve` | Run the service (discovery + bridge emulation). Default. |
| `setup` | Pair with the Bridge Pro (fetch app key, pin certificate). |
| `discover` | Find the Bridge Pro via the Philips cloud. |
| `avahi-service` | Emit an Avahi service file (see the avahi caveat below). |
| `version` | Print the version. |

## Flags (`serve`)

| Flag | Default | Purpose |
|------|---------|---------|
| `-mode` | `rest` | Control mode: `rest` (per-light REST-follow) or `entertainment` (low-latency DTLS stream to the Pro). See [docs/DESIGN.md](docs/DESIGN.md#control-modes). |
| `-http-port` | `80` | HTTP port the TV connects to. |
| `-advertise-ip` | auto | IP advertised via mDNS/SSDP; set it on a multi-homed host. |
| `-bridge-ip` | — | Bridge Pro IP (skips cloud discovery). |
| `-idle-off-timeout` | `30s` | When the TV stops driving the lights for this long, flash them and turn them off (the TV sends no off signal, it just goes silent). `0` disables. |
| `-entertainment-dtls-timeout` | `5s` | Entertainment mode: how long to wait, after confirming the TV's stream activation, for the TV to open its DTLS stream before reverting to REST-follow. Raise it if a TV opens its stream slower. |
| `-skip-tls-verify` | off | Skip Bridge Pro certificate pinning (fallback). |
| `-debug` | off | SSDP/HTTP diagnostics + mDNS observer. |

Discovery-debugging flags (`-identity-profile`, `-ssdp-*`, `-description-profile`,
`-discovery-burst-*`) are documented in [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

## Troubleshooting

### Entertainment stream: re-trigger after a relume restart
In `-mode entertainment` the TV — not relume — opens the DTLS stream, and only after relume
confirms its stream activation. Restarting the relume container mid-session orphans that session:
the TV falls back to polling `GET /api/{user}/lights/1` without re-creating the entertainment
group, so the lights go idle (and the idle-off monitor turns them off).

To reconnect, **toggle Ambilight off and on again on the TV** (the Ambilight feature itself —
*not* Ambilight+Hue). The TV then re-runs the activation handshake. Confirm in the log:
```
ENTERTAINMENT group create requested by TV ...
ENTERTAINMENT stream activation requested by TV ... active=true
entertainment stream connected from=<tv-ip>:...
```

### Cloud suppression
If a real Hue bridge is registered at `discovery.meethue.com`, the TV may resolve it via the
cloud and **skip local discovery** (diyHue #988). Disconnect or block the original bridge for at
least 30 seconds before scanning. Check with `curl https://discovery.meethue.com/` from the TV's
network; the clean local-discovery state is `[]`.

### mDNS conflict with avahi
If the host runs an `avahi-daemon` (it owns UDP 5353), relume's built-in mDNS announcer cannot
bind the port. Either let avahi announce instead:
```bash
docker compose run --rm relume avahi-service -config /data/relume.json > /etc/avahi/services/relume-hue.service
# match the port to the serve http-port: relume avahi-service -http-port 80
```
or disable `avahi-daemon`, then relume's own announcer works.

### Rootless Docker and port 80
A real bridge speaks on port 80. Under **rootless** Docker, ports <1024 require a host sysctl:
```bash
sudo sysctl net.ipv4.ip_unprivileged_port_start=80   # do NOT run the container as root
```
Alternatively use a high port (`-http-port 8080`) — works as long as the TV honors the port
advertised via mDNS (to be verified).

### The TV does not find relume
This is the hard part — a powered-on Bridge Pro on the same LAN usually wins over relume, and the
TV exercises several discovery stacks in one scan. See
[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) for the capture procedure and the experimental
identity/SSDP flags.

## Persistence / secrets

State (bridge identity, TV tokens, **Bridge Pro app key + client key**) lives in
`./data/relume.json`. This file holds secrets — do not share or commit it (it is gitignored).

## Build / test (local)

```bash
go build -o relume ./cmd/relume
go test ./...
```
