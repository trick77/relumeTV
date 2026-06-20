# Troubleshooting

This guide covers two things: the everyday operational issues below, and the harder,
developer-facing problem further down — getting the TV to discover relumeTV in the first place.
The [README](../README.md) has a one-paragraph summary of the single most common blocker.

## Common operational issues

### Entertainment stream: re-trigger after a relumeTV restart
In `-mode entertainment` the TV — not relumeTV — opens the DTLS stream, and only after relumeTV
confirms its stream activation. Restarting the relumeTV container mid-session orphans that session:
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
If the host runs an `avahi-daemon` (it owns UDP 5353), relumeTV's built-in mDNS announcer cannot
bind the port. Either let avahi announce instead:
```bash
docker compose run --rm relumetv avahi-service > /etc/avahi/services/relumetv-hue.service
# match the port to the serve http-port: relumetv avahi-service -http-port 80
```
or disable `avahi-daemon`, then relumeTV's own announcer works.

### Rootless Docker and port 80
A real bridge speaks on port 80. Under **rootless** Docker, ports <1024 require a host sysctl:
```bash
sudo sysctl net.ipv4.ip_unprivileged_port_start=80   # do NOT run the container as root
```
Alternatively use a high port (`-http-port 8080`) — works as long as the TV honors the port
advertised via mDNS (to be verified).

## Discovery: the hard part

The single biggest blocker is **coexistence with a powered-on Bridge Pro**. The real Pro also
announces `_hue._tcp` (as `Hue Bridge - XXXXXX` / `modelid=BSB003`), and the TV appears to
de-duplicate and prefer it. Measured: power the Pro **off** and the TV instantly lists relumeTV
and sends `POST /api`; power it on and relumeTV is filtered out. Winning over a powered-on Pro is
an open problem. (relumeTV proxies control *to* the Pro, so testing with the Pro off only
validates discovery and pairing — not light control.)

What the current Philips Android TV actually does during an Ambilight+Hue search (measured):

- It does **not** send a Hue-specific SSDP M-SEARCH and does **not** query
  `discovery.meethue.com`.
- After a TV reboot it actively queries `_hue._tcp.local` and fetches plain `/description.xml`
  through the Android/Dalvik stack, then later sends a `MediaServer:1` SSDP M-SEARCH and fetches
  `/description.xml?relumetv=ms1` through the Philips DLNA stack.

So **mDNS announce is the primary path.** The working reference is hass-emulated-hue: the mDNS
instance name must be exactly `Philips Hue - XXXXXX` (last 6 of the bridgeid, spaces around the
dash) with TXT bridgeid + modelid. diyHue's `DIYHue-XXXXXX` name is not found by the TV. SSDP is
still served (rootdevice, uuid, basic) but is secondary.

The mDNS announcer must **register exactly once** and never re-announce via a library `Shutdown`
that multicasts an mDNS goodbye (TTL 0) — that evicts relumeTV from the TV's cache and it flickers
out of the Ambilight list.

## Capturing a discovery session

Run a short announcement burst on the Linux/NAS host while the TV is inside its Ambilight+Hue
bridge search, with a packet capture alongside:

```bash
relumetv serve -debug -advertise-ip <nas-lan-ip> -tv-ip <tv-ip> \
  -discovery-burst-duration 90s -discovery-burst-interval 1s

sudo tcpdump -ni <iface> 'host <tv-ip> or udp port 5353 or udp port 1900 or tcp port 80'
```

Expected signals:
- **Passive mDNS:** relumeTV logs `mdns: burst re-announced as hue bridge`; the TV may then connect
  to `/description.xml` or `/api` without first sending a query.
- **Active mDNS:** relumeTV logs `mdns: query` from `-tv-ip` (even for non-Hue question names).
- **SSDP:** relumeTV logs the TV M-SEARCH and responds immediately; tcpdump shows a follow-up
  `GET /description.xml`.

## Experiment history

Each row is a hypothesis tested against the real TV. None has reached `POST /api` with a
powered-on Pro yet. The experimental identity/descriptor flags named in the rows below
(`-identity-profile`, `-description-profile`, `-ssdp-media-server-alias`,
`-ssdp-media-server-basic-body`, `-ssdp-descriptor-variants`) have since been removed: once
the confirmed-working identity (mDNS `BSB002`, `description.xml` served as `text/xml`,
register-once) was established, the knobs were no longer needed.

| Version | Variation | Result |
|---------|-----------|--------|
| `0.1.8` | Ambilight identity profile, OSS-emulator headers, short CLIP v1 config + compatibility endpoints. | TV stopped after descriptor discovery. |
| `0.1.9` | HTTP `Server`/`Cache-Control` on `description.xml`; MediaServer alias `max-age=1`. | No `/api` follow-up. |
| `0.1.10` | mDNS SRV host changed to lower bridgeid (`<bridgeid>.local.`). | TV HTTP `Host` stayed the IP → hostname multiplexing not useful. |
| `0.1.11` | Ambilight serial, UDN, SSDP UUID/USN changed to lower bridgeid with `FFFE`. | No `/api` follow-up. |
| `0.1.12` | Basic:1 SSDP USN changed to `uuid::<urn:...:basic:1>`. | After reboot the TV fetched plain `/description.xml` and `?relumetv=ms1`; still no `/api`. |
| `0.1.13` | Added `-ssdp-descriptor-variants` (`?relumetv=basic1` Basic body). | Windows Chromium/DIAL fetched `basic1`; the TV fetched only plain `/description.xml` and `?relumetv=ms1`. Still no `/api`. |
| `0.1.15` | Added `-description-profile ambilight-reference`. | TV fetched the changed `?relumetv=ms1` bytes; still no `/api`. |
| `0.1.16` | Added `-ssdp-media-server-basic-body`. | Basic body from the `?relumetv=ms1` URL. |
| next | mDNS register-once; removed Shutdown-based re-announce (it emitted goodbye/TTL-0 packets that evicted the bridge from the TV cache). | Root cause of the flicker; the confirmed-working 83noit emulator registers once. |
