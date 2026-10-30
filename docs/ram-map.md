# RAM map (application)

Recovered from how the code uses each address. All of these are inferred names.

## Motion state struct at `0x20000154` (`g_154`)

| Offset | Size | Meaning |
|---|---|---|
| `+0x00` | u8 | Pattern update period (ms); `main` runs `0x34B0`/`0x6A80` when `g_034` exceeds it |
| `+0x01` | u8 | X step period (ms), compared with `g_036` |
| `+0x02` | u8 | Y step period (ms), compared with `g_038` |
| `+0x03` | u8 | Play mode: 0 auto, 1–3 random small/medium/large, 4 stopped |
| `+0x04` | s8 | X step size per update (`0x4A7C` sets 1) |
| `+0x05` | s8 | Y step size per update |
| `+0x06` | u8 | Random dwell value |
| `+0x07` | u8 | Boot-sweep sub-state |
| `+0x08` | u8 | Dwell active flag |
| `+0x09` | u8 | Boot-sweep phase (0–2 running, 3 done) |
| `+0x0E` | u16 | Dwell counter |
| `+0x10` | u16 | Current X PWM compare (written to ADT1 CH2) |
| `+0x12` | u16 | Current Y PWM compare (ADT1 CH3) |
| `+0x14` | u16 | Target X |
| `+0x16` | u16 | Target Y |

`0x41D0(speed, keep)` sets `+0x00..+0x02` to `speed`; when `keep == 0` it also clears the three tick counters.

## Globals

| Address | Meaning |
|---|---|
| `g_000` | SystemCoreClock |
| `g_006` | Raw button mask (current sample) |
| `g_007` | Debounced button mask |
| `g_008` / `g_009` | Last pressed mask / release event |
| `g_00A` | Long-press active (> 1 s) |
| `g_00C` | Debounce timer (ms) |
| `g_00E` | Long-press action done |
| `g_00F` | Nudge repeat timer (ms) |
| `g_010` | Offline mode (button held at power-up) |
| `g_011` | Manual mode active (a direction key is held) |
| `g_012` | Calibration-cross step (0–5, `0xFF` idle) |
| `g_013` / `g_014` | X / Y target reached |
| `g_015` / `g_016` | Power-up button sample counters |
| `g_017` | Laser blink counter (calibration feedback) |
| `g_018` | Seconds until next heartbeat (30) |
| `g_01A`–`g_022` | RX frame parser state |
| `g_024` / `g_026` | Calibrated centre X / Y (persisted) |
| `g_02A` | Auto-pattern phase; `0xFFFF` = not in auto. DP `0x66` accepted only when > 350 |
| `g_02C` | Auto-pattern step (0–89) |
| `g_02E` | **Seconds since play started**: the 5-minute counter |
| `g_030` | "Mode 1 runs the auto pattern" flag |
| `g_032` | Blink timer (ms) |
| `g_034` / `g_036` / `g_038` | Pattern / X-step / Y-step tick counters (ms) |
| `g_03A` / `g_03C` | Last DP `0x66` X / Y |
| `g_03E` | ms → s prescaler |
| `g_040` | ms counter (saturates), used as a 500 ms start guard |
| `g_042` / `g_045` | Version bytes (MCU / protocol) |
| `g_049` | OTA pending: stops watchdog feeding |
| `g_04A` | Software clock: `[0]` minute, `[1]` hour, `[2]` day |
| `g_054` / `g_058` | UART RX ring write / read pointers |
| `g_05C` | Suppress DP reports when 1 |
| `g_05D` | Reset requested (cmd `0x04`) |
| `g_05F` | Network status from cmd `0x03` |
| `g_060` | First heartbeat sent |
| `g_069` | Main-loop counter for the OTA-flag check |
| `g_074[12]` | Timer fired-today flags |
| `g_10A` | Timer blob (74 bytes) reported as DP `0x67` |
| `g_10C` | 12 timer entries × 6 bytes: `[0]` id, `[1]` enabled, `[2..3]` start minutes, `[4..5]` mode |
| `g_16C` | UART RX ring buffer (0x133 bytes) |
| `g_29F` | Current RX frame |
| `g_3D2` | TX frame buffer |
