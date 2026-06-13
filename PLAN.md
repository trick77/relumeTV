# relume — Plan & Stand

Software-Bridge, die einen **Philips Ambilight-TV** mit einer **Hue Bridge Pro (BSB003)**
verbindet, indem sie sich gegenüber dem TV als alte Gen-2-Bridge (BSB002) ausgibt und alle
Befehle über HTTPS/CLIP-v2 an die echte Bridge Pro weiterreicht.

```
Ambilight-TV  ──HTTP:80 + SSDP + DTLS:2100──▶  relume  ──HTTPS:443 + DTLS:2100──▶  Bridge Pro  ──Zigbee──▶  Lampen
```

## Warum

Die Bridge Pro bricht den Ambilight+Hue-Pfad an drei Stellen:
1. **Kein SSDP/UPnP mehr** — nur noch mDNS + Cloud; die TV-Firmware sucht aber per SSDP `M-SEARCH` und erwartet `/description.xml`.
2. **Nur HTTPS:443** — kein Plain-HTTP:80; die TV-Firmware ist auf HTTP verdrahtet.
3. **Nur CLIP v2** — die v1-Discovery-/Pairing-Pfade des TVs lösen nicht mehr auf.

**Ground Truth** (verifiziert via diyHue-Quellcode, das nachweislich Ambilight-TVs bedient):
Der TV versucht **zuerst die Entertainment-API** (DTLS-PSK über UDP 2100, ~25–50 Hz, binäre
`HueStream`-Frames) und fällt nur auf **CLIP-v1-REST** (`PUT /groups/{id}/action`, ~10 Hz,
drosselt → laggy) zurück. Der bei der Kopplung erzeugte `clientkey` ist der DTLS-PSK.

## Entscheidungen

| Thema | Entscheidung |
|------|--------------|
| Basis | Eigenständiger Go-Proxy (diyHue nur als Referenz, kein Fork) |
| Sprache | Go |
| Deployment | Docker mit `--network=host` (SSDP-Multicast braucht dasselbe L2-Netz) |
| Lampen | Live von Bridge Pro proxen |
| Pfad | Voll: Entertainment + REST-Fallback |
| Bridge-Pro-Setup | Einmalige Kopplung; TLS-Zertifikat pinnen (Default), `skip-verify` als Fallback |

## Architektur

**Frontend (TV-seitig, emuliert BSB002):**
- `internal/ssdp` — Multicast-Responder (M-SEARCH) + NOTIFY ssdp:alive. Header verifiziert:
  `SERVER: Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0`, `hue-bridgeid`, `USN: uuid:2f402f80-...`.
- `internal/upnp` — `/description.xml` mit `modelName=Philips hue bridge 2015`, `modelNumber=BSB002`.
- `internal/clipv1` — HTTP:80: Pairing (`POST /api`, Link-Fenster), `config`, Datastore,
  Lampen/Gruppen, Stream-Aktivierung.
- `internal/entertainment` — DTLS-PSK-Server UDP 2100 (PSK = TV-clientkey). *[M4]*
- `internal/huestream` — HueStream-Frames parsen + encodieren (v1 16B / v2 52B-Header). *[M4]*

**Backend (Bridge-Pro-seitig, agiert als Hue-App):**
- `internal/bridgepro` — CLIP-v2-Client (HTTPS:443): Ressourcen lesen, REST-Steuerung,
  Entertainment-Config aktivieren; DTLS-PSK-Client zur Pro. TLS gepinnt. *[M2–M4]*

**Kern:**
- `internal/config` — persistenter Zustand: Identität, TV-Tokens, Pro-Kopplung, Mapping.
- `internal/translate` — v1↔v2-Übersetzung + Lampen-/Channel-Mapping. *[M2+]*
- `internal/bridge` — Lifecycle/Verdrahtung Frontend↔Backend. *[M2+]*
- `cmd/relume` — `serve` (Default), `link` (Pairing-Fenster), `setup` (Pro koppeln *[M2]*).

**Datenfluss Entertainment:** TV koppelt → liest Lampen/Groups → `PUT /groups/{id}
{"stream":{"active":true}}` → relume aktiviert Entertainment-Config auf der Pro, öffnet
DTLS-Client zur Pro:2100 und DTLS-Server für den TV → TV-Frames werden geparst, ge-remappt,
als HueStream-v2 an die Pro gestreamt. Stream-Stop → deaktivieren.

## Meilensteine

- **M1 — Discovery & Pairing** ✅ **FERTIG & verifiziert.** TV findet & koppelt die Bridge.
- **M2 — Bridge-Pro-Anbindung** ✅ **FERTIG & am echten Gerät (BSB003) verifiziert.**
  `bridgepro`-CLIP-v2-Client (HTTPS + Cert-Pinning), `discover`- und `setup`-Befehl
  (Link-Button an der Pro → App-Key + clientkey holen), `translate` v2→v1-Lampenliste.
  Verifiziert: HTTPS-only-Annahme bestätigt (HTTP:80 → 301), Pinning, Pairing, 16 Lampen
  über den Proxy als v1-Liste.
- **M3 — REST-Fallback** ✅ **Lampen-Steuerung FERTIG & am echten Gerät verifiziert.**
  `PUT lights/{id}/state` → v1→v2-Übersetzung → CLIP-v2-PUT an die Pro (207/errors-Array
  ausgewertet). Request-Logging ergänzt (um reales TV-Verhalten zu beobachten).
  Reale Lampe über den Proxy geschaltet. **Offen:** Gruppen-Pfad (`groups/{id}/action`,
  Gruppen-Listing) ist noch ein geloggter Stub — wird mit M4 anhand des realen TV-Verhaltens
  vervollständigt.
- **Discovery (mDNS)** ✅ **implementiert** (`internal/mdns`, aktiver `_hue._tcp`-Announce
  als `Philips Hue - XXXXXX`/BSB002) + `avahi-service`-Befehl für avahi-Hosts. **Befund am
  realen TV (siehe unten).** Verifizierung der TV-Erkennung steht noch aus (auf Linux-Ziel).
- **M4 — Entertainment** ⏳ offen. `huestream` (+Tests), DTLS-Server (TV) + DTLS-Client (Pro),
  Entertainment-Config-Aktivierung, Stream-Forwarding. *Ziel:* flüssiges Ambilight.
- **M5 — Packaging** ✅ **FERTIG.** Dockerfile (statisch, multi-stage), `docker-compose.yml`
  (`network_mode: host`), Persistenz unter `./data`, README mit Deployment + Caveats.
  Docker-Image baut.

## Discovery-Befund (am realen Philips-TV gemessen)

- Der TV (IP .112) sendet SSDP-M-SEARCH **nur** für `MediaServer` (DLNA) — **nie** etwas
  Hue-bezogenes, nie `/description.xml`/`/api`. **→ Der TV nutzt für die Hue-Suche kein SSDP.**
- Die echte Bridge Pro **announct selbst mDNS** `_hue._tcp` als `Hue Bridge - XXXXXX`,
  `modelid=BSB003`. relume announct `Philips Hue - XXXXXX`, `modelid=BSB002` (Format von
  hass-emulated-hue, das vom TV gefunden wird; diyHue-Name wird NICHT gefunden — #988).
- **macOS-Testumgebung untauglich:** System-`mDNSResponder` belegt Port 5353 → relumes
  Go-zeroconf-Announce greift dort nicht (Workaround im Test: `dns-sd -R` über System-Bonjour).
  Auf single-homed Linux ohne/mit konfiguriertem avahi sauber → finaler TV-Test dort.
- Mögliche Cloud-Suppression: `relume discover` lieferte die echte Bridge aus der Philips-Cloud.
  Falls der TV trotz korrektem mDNS nichts findet → `discovery.meethue.com` per DNS umbiegen.

## Offene Punkte (am echten Gerät verifizieren)

- **TV-Erkennung von relume per mDNS auf dem Linux-Ziel** (entscheidender offener Test).
- Exaktes `HueStream`-v2-Layout (52-Byte-Header, Channel-Chunks).
- Genaue CLIP-v2-Calls zum Anlegen/Aktivieren der `entertainment_configuration` auf der Pro.
- Ob der TV ein bestimmtes `swversion`/`apiversion` braucht, um Entertainment zu versuchen.
- Exakter `devicetype`-String, den der TV bei `POST /api` schickt; ob der TV den per mDNS
  beworbenen Port nutzt oder 80 hartcodiert.

## Aktueller Stand (M1)

Implementiert und getestet:
- `internal/config` — stabile Identität (12-Hex-Serial → BridgeID/UUID), persistenter JSON-Zustand, ApiUser-Verwaltung.
- `internal/ssdp` — Multicast-Listener + sofortige M-SEARCH-Antwort + periodisches NOTIFY.
- `internal/upnp` — `/description.xml` mit BSB002-Kennung.
- `internal/clipv1` — Pairing mit 30s-Link-Fenster (Fehler 101 ohne Druck), `username`+`clientkey`,
  `config` (`modelid=BSB002`), Datastore, Lampen/Gruppen als Platzhalter; Web-UI + `/link`.
- `cmd/relume` — `serve`/`link`, IP-Auto-Detektion.

### Bauen & Laufen lassen

```bash
go build ./...
go test ./...

# Lokaler Smoke-Test (Port 80 braucht root; hier 8080):
go run ./cmd/relume serve -http-port 8080 -advertise-ip <deine-ip> -config ./relume.json

# In Produktion (Port 80, Discovery via SSDP):
sudo ./relume serve            # oder via Docker --network=host

# Pairing-Fenster öffnen (statt physischem Link-Button):
./relume link                  # oder Web-UI: http://<bridge-ip>/
```

### Verifiziert (Smoke-Test)
- `/description.xml` liefert BSB002 + konsistente UUID.
- Pairing ohne Link-Button → Fehler `type:101`.
- Pairing nach Link-Druck → `username` (32 Hex) + `clientkey` (32 Hex uppercase).
- `/api/{user}/config` → `modelid=BSB002`, `bridgeid` konsistent zur Serial.
