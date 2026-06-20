# TODO

## README.md — capture TV screenshots for documentation

The future `README.md` should show the Ambilight+Hue flow on the TV (bridge
discovery, the bulb list, light-position assignment). The test TV is a Philips
**65OLED806** (Android TV 11) running in **developer mode**, so screenshots can be
pulled over ADB from a computer on the same LAN.

### 1. Connect ADB over the network

On the TV, under **Developer options**, enable **ADB debugging** / **USB debugging**,
then from the computer:

```bash
adb connect 192.168.178.112:5555
```

Confirm the RSA fingerprint prompt on the TV the first time. If network ADB is not
available on this model, use ADB over USB instead.

### 2. Pull a screenshot

```bash
adb -s 192.168.178.112:5555 exec-out screencap -p > tv-screenshot.png
```

If `exec-out` misbehaves (old ADB version, CRLF issues), fall back to:

```bash
adb shell screencap -p /sdcard/screen.png
adb pull /sdcard/screen.png .
```

### Caveats

- Requires `adb` locally (`brew install android-platform-tools` on macOS).
- DRM/protected content (active streaming) often captures as a black frame — UI
  screens like the Ambilight+Hue menu are fine.
- If `adb connect` returns "connection refused", network ADB is off on the TV; some
  Philips devices need a toggle-and-reboot, or only allow it over USB.

### Screens worth capturing for the README

- [ ] Ambilight+Hue: bridge discovered ("relumeTV" in the bridge list)
- [ ] Bulb list showing the paired Hue color bulbs
- [ ] Light-position assignment screen
- [ ] Ambilight running with the lights following the screen
