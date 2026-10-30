# Firmware overview

## Flash layout

| Range | Content |
|---|---|
| `0x0000–0x1FFF` | Bootloader, own vector table at `0x0000` (SP `0x200007A0`, reset `0x00DC`) |
| `0x2000–0x6FFF` | Application, vector table at `0x2000` (SP `0x20000610`, reset `0x20D4`) |
| `0x6CB8` | DP id/type table (see [tuya-protocol.md](tuya-protocol.md)) |
| `0x7A00` | Persistent settings, 79 bytes (see below) |
| `0x7C00` | Bootloader OTA transfer state (erased in this dump) |
| `0x7E00` | OTA flag sector: `0x55`, `"gcslbvj0"`, version bytes |

### Settings record at `0x7A00`

Read by `0x3B58` at boot, written by `0x3FC8`.

| Offset | Size | Meaning |
|---|---|---|
| 0 | 2 | Servo X centre (`g_024`), default `0x031B` = 795 |
| 2 | 2 | Servo Y centre (`g_026`), default `0x031B` |
| 4 | 74 | Timer blob (`g_10A`: 2 header bytes + 12 × 6-byte entries at `g_10C`) |
| 78 | 1 | Checksum = (sum of bytes 0..77) + 1 |

If the checksum fails, the timers are cleared and both centres fall back to `0x031B`.

## Boot flow

1. **Bootloader `main` (`0x1174`)**: clock/peripheral init, then checks the byte at `0x7E00`.
   - `0x55` → Tuya OTA receive mode (`0x1924`). The new image is written from `0x2000` and verified with CRC32 at the end, then started.
   - Otherwise → `0x1144` checks only that the app's initial SP lies in RAM, then jumps to the app reset vector at `0x2004`.
     No CRC is checked on a normal boot.

   Details: [bootloader.md](bootloader.md).
2. **Application reset (`0x20D4`)** → C runtime init → `main` at `0x6398`.

## Pins

Port offsets used by the GPIO helpers: `0x00` = P0, `0x40` = P1, `0x80` = P2, `0xC0` = P3.

| Pin (TSSOP20) | Port | Use in firmware | README label |
|---|---|---|---|
| 1 | P3.4 | Input, pull-up, button bit1 | start |
| 2 | P3.5 | Input, pull-up, button bit2 | left |
| 3 | P3.6 | Input, pull-up, button bit3 | right |
| 5/6 | P0.1/P0.2 | XTH crystal | XTAL |
| 10 | P0.3 | Input, pull-up, button bit4 | up |
| 11 | P1.5 | UART0 RXD (AF6) | UART RX |
| 12 | P1.4 | UART0 TXD (AF6) | UART TX |
| 13 | P2.3 | Input, pull-up, button bit5 | down |
| 15 | P2.5 | ADT1 (TIM5) PWM, servo | – |
| 16 | P2.6 | ADT1 (TIM5) PWM, servo | – |
| 19 | P3.2 | Output, laser/LED | led |
| 20 | P3.3 | Input, pull-up, button bit0 | button |

## Peripheral setup (called from `main`)

| Function | Does |
|---|---|
| `0x2BE8`, `0x2C7A`, `0x2C40` | Clock setup (XTH, RCH trim, SysTick-based delays `0x5E90`) |
| `0x2C0E` | P3.2 output, initially low |
| `0x2D28` | Button inputs with pull-up |
| `0x5308` | UART RX ring buffer pointers |
| `0x2CE0` | P1.4/P1.5 → UART0 |
| `0x2CAE` | UART0 init, RX interrupt enabled |
| `0x36AC(8)` | Flash wait/timing configuration |
| `0x2B44` | P2.5/P2.6 → ADT1 channels |
| `0x2A64(10000, cx, cy)` | ADT1 PWM, period 10000, compare = centre |
| `0x2B86` | Base timer 2, periodic interrupt (1 ms tick) |
| `0x2D80` | Watchdog, reset mode |

## Timing

The base-timer-2 interrupt (vector `TIM2` → `0x462C` → `0x4708`) increments the millisecond counters `g_00C`, `g_00F`, `g_032`, `g_034`, `g_036`, `g_038`, `g_040`, and calls `0x4654`, which:

- counts `g_03E` to 1000, then increments the seconds-since-play counter `g_02E`
- keeps a software clock in `g_04A` (minute, hour, day); at midnight it clears the per-timer "already fired today" flags `g_074[12]`
- every 30 s sends a `0xE1` heartbeat to the Tuya module (`0x5330(2)`)

## Main loop (`0x6398`)

Before the loop:

- Moves the servos to the stored centre.
- Samples the button (P3.3) until 101 identical readings: **held at power-up → offline mode** (`g_010 = 1`, laser on); otherwise Tuya mode (`g_010 = 0`).
- Mode = 4 (stopped), starts the watchdog.

Each iteration:

1. If no OTA is pending (`g_049 == 0`): feed the watchdog (`0x48F8`); every 100 iterations clear a stale OTA flag at `0x7E00` (`0x3680`).
2. While the boot sweep runs (`g_154[9] < 3`): run it (`0x3C74`) every ~3 ms.
3. After the sweep:
   - `0x404C` – debounce the six inputs into `g_007` and produce events in `g_009`.
   - `0x39FC` – act on button events (long press, start/stop, blink feedback).
   - `0x315C` – manual nudging and the calibration sweep.
   - `0x4990` – step the current PWM position toward the target, then write ADT1 (`0x3424`).
   - Every `g_154[0]` ms: `0x34B0` (timers + 5-minute stop) and `0x6A80` (pattern generator).
4. In Tuya mode: `0x53E0` – parse and dispatch UART frames.

## Buttons

`0x404C` builds a bit mask (bit set = pressed) and debounces it:

| Bit | Value | Pin | Effect (`0x315C` / `0x39FC`) |
|---|---|---|---|
| 0 | `0x01` | P3.3 button | Short press: start play (Tuya mode: mode 1 after 500 ms guard; offline: mode 3). Hold > 1 s then > 5 s: clear timers and blink the laser 5 times. |
| 1 | `0x02` | P3.4 start | Save current position as the new centre to flash, then run a 4-point calibration cross |
| 2 | `0x04` | P3.5 left | X + 4 per press (+1 every 50 ms while held long) |
| 3 | `0x08` | P3.6 right | X − 4 |
| 4 | `0x10` | P0.3 up | Y + 4 |
| 5 | `0x20` | P2.3 down | Y − 4 |

Nudging is limited to ±100 around the centre. Pressing any key other than the button forces mode 4 (manual) and turns the laser on.

## Play modes (`g_154[3]`)

Dispatched by `0x6A80`:

| Mode | Behaviour |
|---|---|
| 0 | "Auto" (`0x5594`): four quarter-circle arcs of radius 200 drawn with soft-float sin/cos (90 steps each), then return to centre and blink the laser 4 times; phase kept in `g_02A`, step in `g_02C` |
| 1 | Random points (`0x3F1C(1)` → `0x3D78`), Y drawn from 0..149 |
| 2 | Random points, Y drawn from 0..249 |
| 3 | Random points, Y drawn from 0..399 |
| 4 | Stopped / manual; servos hold their target |

How the random points in modes 1–3 are generated:

- The generator is an LCG (`0x21EC`: multiplier `0x41C64E6D`, increment `0x3039`).
- X is always `rand % 400` (`0x4A8C`). Y is `rand % 150 / 250 / 400` depending on the mode.
- Both are relative to (centre − 200), so 200 is the centre.
- The point is clamped to a circle of radius 200 (r² = `0x9C40`) by shrinking the shorter axis.
- With an odd random dwell value, the laser holds still for a while before the next point.

The effect is that mode 1 keeps the dot in the lower 150/400 of the Y range, while mode 3 uses the whole circle.

## Position limits

There is no configurable height or range limit in the firmware. All writes to the target Y (`g_154+0x16`) are in `0x315C`, `0x34B0`, `0x39FC`, `0x3D78`, `0x4200`, `0x5594`, `0x6398` and the DP handlers, and none of them applies an upper bound. The only effective limits are these:

| Source | Limit |
|---|---|
| Direction buttons (`0x315C`) | ±100 around the calibrated centre |
| Random modes 1–3 (`0x3D78`) | Y band of 150 / 250 / 400 from one edge, clamped to radius 200 |
| Auto pattern (`0x5594`) | arcs of radius 200 |
| DP 102 / manual (`0x5F80`) | **none**: any offset from 200 is applied as is |
| Boot sweep (`0x3C74`) | Y moves once to centre − 200 and back |

In the `SERIAL.md` capture, the Tuya app only sent Y values 64–236. So any range option in the app most likely clips manual positions on the app side. DP 107 (`0x6B`) is in the DP table but not handled (see [tuya-protocol.md](tuya-protocol.md#data-points)). If the app option sends that DP, the MCU ignores it. Unverified: capture the UART while changing the option to confirm.
