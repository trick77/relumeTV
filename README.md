# relume

A software bridge that connects a **Philips Ambilight TV** to a **Hue Bridge Pro (BSB003)**.
relume presents itself to the TV as an old gen-2 bridge (BSB002) and proxies every request
to the real Bridge Pro over HTTPS/CLIP v2.

```
Ambilight TV  ──mDNS/SSDP + HTTP──▶  relume  ──HTTPS/CLIP v2──▶  Hue Bridge Pro  ──Zigbee──▶  lights
```

Background and design: see [PLAN.md](PLAN.md) and [AGENTS.md](AGENTS.md).

## Requirements

- relume must run on the **same L2 network** as the TV (discovery uses multicast).
  → Docker requires `network_mode: host`.
- A reachable Hue Bridge Pro on the same network.

## Quick start (Docker)

```bash
# 1. Pair with the real Bridge Pro (once). When prompted, briefly TAP the link
#    button on the Bridge Pro (do not hold it).
docker compose run --rm relume setup -config /data/relume.json
#    add -bridge-ip <ip> if cloud discovery finds nothing.

# 2. Start the service
docker compose up -d

# 3. On the TV, start the Ambilight+Hue bridge search. When the TV asks for the
#    link button, open the pairing window:
docker compose run --rm relume link        # or in a browser: http://<host-ip>/
```

The image is pulled from `ghcr.io/trick77/relume` (built by the release workflow).
To build locally instead: `docker build -f Containerfile -t relume:dev .`

## Commands

| Command | Purpose |
|---------|---------|
| `serve` | Run the service (discovery + bridge emulation). Default. |
| `setup` | Pair with the Bridge Pro (fetch app key, pin certificate). |
| `discover` | Find the Bridge Pro via Philips cloud. |
| `link` | Open the pairing window (30s) for the TV. |
| `avahi-service` | Emit an Avahi service file (see mDNS caveat). |
| `version` | Print the version. |

Useful `serve` flags: `-http-port` (default 80), `-advertise-ip` (empty = auto),
`-debug` (SSDP/HTTP diagnostics + mDNS observer), `-tv-ip` (log all mDNS
questions from that TV), `-discovery-burst-duration`, `-discovery-burst-interval`.

## Important caveats

### Discovery: diagnose both passive and active paths
Measured against the current test TV: Hue search did not send Hue-specific SSDP
M-SEARCH, did not fetch `/description.xml`, and did not actively query `_hue._tcp`.
That points to passive mDNS listening for the bridge announcement. Public diyHue
reports also show some Philips TVs sending generic SSDP M-SEARCH and then fetching
`/description.xml`, so relume keeps both paths active.

For a decisive capture on Linux/NAS, run a short announcement burst while the TV is
inside Ambilight+Hue bridge search:

```bash
relume serve -debug -advertise-ip <nas-lan-ip> -tv-ip <tv-ip> \
  -discovery-burst-duration 90s -discovery-burst-interval 1s

sudo tcpdump -ni <iface> 'host <tv-ip> or udp port 5353 or udp port 1900 or tcp port 80'
```

Expected signals:
- Passive mDNS path: relume logs `mdns: burst re-announced as hue bridge`; the TV may
  then connect to `/description.xml` or `/api` without first sending a query.
- Active mDNS path: relume logs `mdns: query` from `-tv-ip`, even for non-Hue question
  names.
- SSDP path: relume logs the TV M-SEARCH and responds immediately; tcpdump should show
  a follow-up `GET /description.xml`.

relume announces `Philips Hue - XXXXXX` / `modelid=BSB002`. The real Bridge Pro
announces itself as `BSB003`, which the TV likely rejects as incompatible.

**mDNS conflict with avahi:** if the host runs an `avahi-daemon` (it owns UDP 5353),
relume's built-in mDNS announcer cannot bind the port. In that case let avahi announce:
```bash
docker compose run --rm relume avahi-service -config /data/relume.json > /etc/avahi/services/relume-hue.service
# match the port to the serve http-port: relume avahi-service -http-port 80
```
Alternatively disable `avahi-daemon`, then relume's own announcer works.

### Cloud suppression
If a real Hue bridge is registered at `discovery.meethue.com`, the TV may resolve it via
the cloud and **skip local discovery** (diyHue #988). Disconnect or block the original
bridge for at least 30 seconds before scanning. Check with
`curl https://discovery.meethue.com/` from the TV's network; the clean local-discovery
state is `[]`.

### Rootless Docker and port 80
A real bridge speaks on port 80. Under **rootless** Docker, ports <1024 require a host sysctl:
```bash
sudo sysctl net.ipv4.ip_unprivileged_port_start=80   # do NOT run the container as root
```
Alternatively use a high port (`-http-port 8080`) — works as long as the TV honors the
port advertised via mDNS (to be verified).

## Persistence / secrets

State (bridge identity, TV tokens, **Bridge Pro app key + clientkey**) lives in
`./data/relume.json`. This file holds secrets — do not share or commit it (it is gitignored).

## Build / test (local)

```bash
go build -o relume ./cmd/relume
go test ./...
```
