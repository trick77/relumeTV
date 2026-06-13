# relume

Software-Bridge, die einen **Philips Ambilight-TV** mit einer **Hue Bridge Pro (BSB003)**
verbindet. relume gibt sich gegenüber dem TV als alte Gen-2-Bridge (BSB002) aus und reicht
alle Befehle per HTTPS/CLIP-v2 an die echte Bridge Pro weiter.

```
Ambilight-TV  ──mDNS/SSDP + HTTP──▶  relume  ──HTTPS/CLIP v2──▶  Hue Bridge Pro  ──Zigbee──▶  Lampen
```

Details und Hintergrund: siehe [PLAN.md](PLAN.md) und [AGENTS.md](AGENTS.md).

## Voraussetzungen

- relume muss im **selben L2-Netz** wie der TV laufen (Discovery nutzt Multicast).
  → Docker zwingend mit `network_mode: host`.
- Eine erreichbare Hue Bridge Pro im selben Netz.

## Schnellstart (Docker)

```bash
# 1. Mit der echten Bridge Pro koppeln (einmalig). Beim Aufruf den Link-Button
#    der Bridge Pro KURZ antippen (nicht halten).
docker compose run --rm relume setup -config /data/relume.json
#    -bridge-ip <ip> falls Cloud-Discovery nichts findet.

# 2. Dienst starten
docker compose up -d

# 3. Am TV die Ambilight+Hue-Bridge-Suche starten. Wenn der TV nach dem
#    Link-Button fragt, das Pairing-Fenster öffnen:
docker compose run --rm relume link        # oder im Browser http://<host-ip>/
```

## Befehle

| Befehl | Zweck |
|--------|-------|
| `serve` | Dienst (Discovery + Bridge-Emulation). Standard. |
| `setup` | Mit Bridge Pro koppeln (App-Key holen, Zertifikat pinnen). |
| `discover` | Bridge Pro per Philips-Cloud im Netz finden. |
| `link` | Pairing-Fenster (30s) für den TV öffnen. |
| `avahi-service` | Avahi-Service-Datei ausgeben (siehe mDNS-Caveat). |

Nützliche Flags für `serve`: `-http-port` (Standard 80), `-advertise-ip` (leer =
auto), `-debug` (SSDP-/HTTP-Diagnose + mDNS-Observer).

## Wichtige Caveats

### Discovery: der TV nutzt mDNS, nicht SSDP
Gemessen am realen Philips-TV: Die Hue-Suche läuft **nicht** über SSDP, sondern über
mDNS (`_hue._tcp`). relume announct das aktiv als `Philips Hue - XXXXXX` / `modelid=BSB002`.
Die echte Bridge Pro announct sich als `BSB003`; der TV verwirft diese als inkompatibel.

**mDNS-Konflikt mit avahi:** Läuft auf dem Host ein `avahi-daemon` (belegt UDP 5353),
kann relumes eingebauter mDNS-Announcer den Port nicht exklusiv nutzen. Dann stattdessen
avahi announcen lassen:
```bash
docker compose run --rm relume avahi-service -config /data/relume.json > /etc/avahi/services/relume-hue.service
# Port an den serve-http-port anpassen: relume avahi-service -http-port 80
```
Alternativ den `avahi-daemon` deaktivieren, dann greift relumes eigener Announcer.

### Cloud-Suppression
Ist eine echte Hue-Bridge bei `discovery.meethue.com` registriert, kann der TV sie per
Cloud auflösen und **lokale Discovery überspringen** (diyHue #988). Prüfen mit
`curl https://discovery.meethue.com/` aus dem TV-Netz — liefert das die echte Bridge,
muss `discovery.meethue.com` per lokalem DNS auf relume umgebogen werden.

### Rootless Docker und Port 80
Eine echte Bridge spricht Port 80. Bei **rootless** Docker sind Ports <1024 nur mit
Host-sysctl bindbar:
```bash
sudo sysctl net.ipv4.ip_unprivileged_port_start=80   # NICHT den Container als root laufen lassen
```
Alternativ auf einen hohen Port wechseln (`-http-port 8080`) — funktioniert, sofern der
TV den per mDNS beworbenen Port respektiert (zu verifizieren).

## Persistenz / Secrets

Der Zustand (Bridge-Identität, TV-Tokens, **Bridge-Pro-App-Key + clientkey**) liegt in
`./data/relume.json`. Diese Datei enthält Geheimnisse — nicht teilen, nicht committen
(ist in `.gitignore`).

## Build / Test (lokal)

```bash
go build -o relume ./cmd/relume
go test ./...
```
