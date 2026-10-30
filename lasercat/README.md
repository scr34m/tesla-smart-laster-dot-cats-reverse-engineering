# lasercat (Go)

Controls the laser cat toy directly from the computer over Bluetooth LE, without the Tuya app or a gateway.

It is a minimal port of [../ble_tuya_client](../ble_tuya_client) that implements only the following:

- the Tuya BLE v3 key exchange and pairing
- two DP writes:

| Command | DP | Value |
|---|---|---|
| `on` / `off` | 105 (bool) | true / false. On starts the built-in arc pattern |
| `small` / `medium` / `large` | 104 (value) | 1 / 2 / 3 – also starts play and resets the 5-minute timer |

Bluetooth access on macOS goes through CoreBluetooth via [`tinygo.org/x/bluetooth`](https://github.com/tinygo-org/bluetooth).

## Build

```
cd lasercat
go build -o lasercat .
```

## Usage

The credentials are three values: uuid, local key and device id. The same `LASERCAT_*` variables are used by `../mcu_ota/ble_ota.py`. They can be passed as flags or environment variables:

```
export LASERCAT_UUID=fc70eab9297bb00b
export LASERCAT_DEVID=bf42ealmnxuvyiyy
export LASERCAT_KEY='<local key>'
export LASERCAT_ADDR=31F8F36B-3F8E-1015-8777-5A8797201D0F   # optional, otherwise the first Tuya device found

./lasercat on
./lasercat large
./lasercat off
```

After sending, the tool prints the DPs the device reports back for two seconds, then disconnects.

On the first run, macOS asks for Bluetooth permission for the terminal app (System Settings → Privacy & Security → Bluetooth).

## Home Assistant bridge (`mqtt`)

```
./lasercat mqtt          # broker defaults to tcp://192.168.1.21:1883
```

The bridge runs continuously, keeps the BLE connection open and reconnects when it drops. Retries back off from 5 s to 2 min. Every minute it sends a status request, which also detects a dead link.

It announces one device, *Laser Cat*, via MQTT discovery:

| Entity | Topics | Values |
|---|---|---|
| `light.lasercat_laser` | `lasercat/light/set`, `lasercat/light/state` (JSON schema) | on/off, effect, colour wheel |
| `sensor.lasercat_status` | `lasercat/status` | text, e.g. `scanning`, `pairing`, `connected`, `disconnected: link lost`, `error: … (retry in 20s)` |

### Light mapping

| Home Assistant | Device |
|---|---|
| on (no colour/effect) | DP 105 = true: built-in arc pattern, shown as effect `on` |
| off | DP 105 = false: centre, laser off |
| effect `small` / `medium` / `large` | DP 104 = 1 / 2 / 3: random play |
| colour wheel | DP 102 = X/Y: laser moves there and stays, shown as effect `manual` |
| effect `manual` | moves back to the last colour-wheel position |

How the colour wheel maps to a position:

- Hue is the direction and saturation is the distance from the calibrated centre. At 100% saturation the distance is `-xy-radius` units (default 200, the same circle the firmware's random modes use).
- DP 102 values are 200 ± offset. The Y axis is inverted in the bridge, so up and down on the wheel match the room.

The firmware ignores DP 102 while the arc pattern (effect `on`) is running. For a colour command in that state, the bridge first sends DP 105 = false, which clears the lock without moving the laser, and then the position.

### Other behaviour

- **Availability:** `lasercat/availability` is `online` only while the BLE link is up. It is also the MQTT last-will topic, so the light turns unavailable when the bridge or the link is down.
- **Auto-off:** the original firmware stops by itself after 5 minutes without reporting it ([../docs/timeout-patch.md](../docs/timeout-patch.md)). The bridge therefore shows the light as off 5 minutes after the last command.
  - **This device runs the patched firmware**, which stops after 510 s and reports DP 104 = 0 as a "finished" marker. The bridge shows the light as off when it receives that report.
  - Run the bridge with `-auto-off 0` so the reported state is used instead of the timer; otherwise the light would show off 3.5 minutes early.
- **Restarts:** discovery is re-published whenever Home Assistant comes back online (`homeassistant/status`).
- **Upgrading:** the earlier `switch`/`select` entities are removed automatically (empty retained discovery config).

MQTT options: `-broker`, `-mqtt-user`, `-mqtt-pass`, `-topic`, `-xy-radius`, or the matching `LASERCAT_*` environment variables.

### Running in the background (launchd)

`~/Library/LaunchAgents/hu.lasercat.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>hu.lasercat</string>
  <key>ProgramArguments</key><array>
    <string>/path/to/lasercat</string><string>-auto-off</string><string>0</string><string>mqtt</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>LASERCAT_UUID</key><string>…</string>
    <key>LASERCAT_DEVID</key><string>…</string>
    <key>LASERCAT_KEY</key><string>…</string>
    <key>LASERCAT_ADDR</key><string>31F8F36B-3F8E-1015-8777-5A8797201D0F</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/tmp/lasercat.log</string>
</dict></plist>
```

`launchctl load ~/Library/LaunchAgents/hu.lasercat.plist`.

macOS asks for Bluetooth permission for the process that uses it. Run the binary once from the terminal first, and grant the permission there if launchd starts it without it.

### Local key

The local key changes every time the device is re-paired with the Tuya app. A stale key shows up as "pairing rejected".

**Do not remove the device from the Tuya app.** Removing it unbinds the BLE module, which deletes the stored key on the device, so no local key works any more. Just close the app or switch off the phone's Bluetooth instead.

The bound state can be seen in the advertisement: Tuya manufacturer data (company `0x07D0`), byte 0, bit 7 set = bound.

### Address

Several Tuya BLE devices may be in range. During testing there were five, all advertising company `0x07D0`. Always set `LASERCAT_ADDR` (this device: `31F8F36B-3F8E-1015-8777-5A8797201D0F`, advertised name `COMMON`); otherwise the first Tuya device found is used.

## Notes

- The address on macOS is the CoreBluetooth UUID, not the MAC address.
- The Go framing and encryption were cross-checked against the Python client in both directions, and both `off` and the MQTT bridge were run against the device.
- Not exercised yet:
  - reconnecting after a dropped link;
  - colour-wheel moves on the real device (the HA → DP mapping is unit-tested).
- Leave the Tuya app disconnected while using this: the module accepts only one BLE connection at a time.
