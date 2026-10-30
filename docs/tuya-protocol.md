# Tuya MCU protocol implementation

## Framing

Standard Tuya serial protocol on UART0 (P1.4 TX / P1.5 RX):

```
55 AA  ver  cmd  lenH lenL  data[len]  checksum
```

`checksum` = sum of all preceding bytes, mod 256.

- **RX**: the UART0 interrupt fills a 0x133-byte ring buffer at `g_16C` (write pointer `g_054`, read pointer `g_058`).
  `0x53E0` pulls bytes (`0x3EE0`), syncs on `55 AA 00` (`0x6554`), collects the frame into `g_29F` and calls the dispatcher `0x5A60`.
- **TX**: `0x5484(cmd, len)` builds the header in `g_3D2`, appends the checksum (`0x62E8`) and sends byte by byte (`0x545C` → `0x6CA8`).

## Product info

Command `0x01` answers with `"gcslbvj0"` + `"1.0.0"` (strings at `0x6B14` / `0x6B20`), matching the capture in `SERIAL.md`.

## Commands handled (`0x5A60`)

Dispatch is on `cmd`. Commands below 9 go through a `switch8` table, `0xE4–0xEE` through a second one.

| cmd | Handler | Meaning |
|---|---|---|
| `0x00` | `0x6360` | Heartbeat. Replies `00` on the first call, `01` afterwards |
| `0x01` | `0x6ADC` | Product info |
| `0x02` | `0x6334` | Work mode: empty reply (the module handles the network LED/reset itself) |
| `0x03` | inline | Network status → `g_05F`; status 0 also clears all timers (`0x6B28`); ACKs with cmd `0x03` |
| `0x04` | inline | Reset request → sets `g_05D = 1` |
| `0x06` | inline | DP command: loops over every DP in the payload and calls `0x5E30` |
| `0x08` | `0x52E4` | Status query: reports DP `0x67` (timers) and sends a heartbeat |
| `0x09` | `0x54C8` | – |
| `0x0E` | inline | Factory test: checks for `"true"` and parses an RSSI from two ASCII digits |
| `0xA1` | `0x5302` | Empty stub |
| `0xE0` | `0x532E` | Empty stub |
| `0xE1` | `0x534C` | Time sync (several sub-formats, year offset 2018 / 2000) |
| `0xE2` | `0x5306` | Empty stub |
| `0xE4–0xE7` | `0x52FC`… | Empty stubs |
| `0xE8` | inline | Replies with version bytes `g_042` / `g_045` |
| `0xE9` | `0x5304` | Empty stub |
| `0xEA–0xEE` | `0x6840` | OTA (see below) |

## Data points

DP table at `0x6CB8` (id, Tuya type). `0x5E30` looks the DP id up, checks that the type matches, and calls `0x5EE8`, which switches on `dpid − 0x65`.

| DP | Type | Handler | Behaviour |
|---|---|---|---|
| `0x65` | raw (0) | `0x6278` | Set the software clock `g_04A` from 6 bytes, only if the day differs or the time is more than 60 min off |
| `0x66` | raw (0) | `0x5F80` | Move to X/Y: two big-endian u16 values, 200 = centre. Target = calibrated centre ± (value − 200), with X mirrored. No clamping. Sets mode 0, laser on, and resets the 300 s counter. **Ignored while `g_02A ≤ 350`**, i.e. while the auto pattern started by DP `0x69` = on is running. DP `0x69` = off sets `g_02A = 0xFFFF` and clears this lock |
| `0x67` | raw (0) | `0x60A0` | Timers: add/modify up to 12 entries (6 bytes each: id, enable, start time, mode/duration), then save to flash and report back |
| `0x68` | value (2) | inline | Play mode: `g_02A = 0xFFFF`, `0x3488(mode)` (resets the 300 s counter, laser on), reports `0x68` back |
| `0x69` | bool (1) | `0x61E4` | On/off. Off: mode 4, centre, laser off. On: mode 0 (auto). Both reset the 300 s counter and report `0x69` back |
| `0x6A` | bool (1) | `0x6044` | Clear all timers, report `0x67` and `0x6A` |
| `0x6B` | raw (0) | – | In the table but not handled by the switch: accepted and ignored. Possibly the app's range/height option (unverified) |

Reporting helpers (all send cmd `0x07`):

| Function | Reports |
|---|---|
| `0x66E4(dp, v)` | bool DP |
| `0x67B4(dp, v)` | value DP (4-byte big-endian) |
| `0x6748(dp, buf, len)` | raw DP |

### Timers

`0x34B0` compares each enabled entry's start time with `hour * 60 + minute` from `g_04A`. When one matches and hasn't fired today (`g_074[i] == 0`), it:

- starts play mode `entry[4]`
- turns the laser on
- resets the 300 s counter

## OTA hand-off

1. Cmd `0xEA` (`0x6840`) erases the `0x7E00` sector and writes `0x55`, then `"gcslbvj0"` and the version bytes from `g_042`.
2. It sets `g_049 = 1`, so `main` stops feeding the watchdog.
3. The watchdog resets the MCU.
4. The bootloader sees `0x55` at `0x7E00` and runs its own UART update routine.

`0xEB–0xEE` are no-ops in the application. The bootloader handles the transfer itself; see [bootloader.md](bootloader.md).
