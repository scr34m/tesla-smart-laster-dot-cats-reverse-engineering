# 5-minute auto-off and the patch

## Original logic (`0x34B0`)

`0x34B0` runs from the main loop once the boot sweep is done.

### Tuya mode, no direction key held

1. It checks the 12 timers (see [tuya-protocol.md](tuya-protocol.md#timers)).
2. Once `g_02E > 300` (seconds since play started), it:
   - sets mode 4 (stopped)
   - sets the target to the calibrated centre
   - turns the laser off (P3.2 = 0)
   - calls `0x4A7C`

**No DP report is sent**, so the app still shows the device as on. This is the second issue in the README.

### Offline mode

The firmware runs offline when the button is held at power-up. The same stop happens at 300 s. At 360 s it restarts with mode 3 and the laser on (`0x345C(3,0)` resets `g_02E`), so offline play cycles 5 minutes on, 1 minute off.

### What resets `g_02E`

`g_02E` is reset by `0x345C`. That function is called when any of these happen:

- DP `0x68` is received
- DP `0x69` is received
- the button is pressed
- a timer fires

DP `0x66` (manual X/Y) also resets `g_02E` directly.

## Patch in `flash_600.bin`

### Change 1 – limit 300 → 510 s (correct)

```
3548: FF 21    movs r1,#0xFF
354A: FF 31    adds r1,#0xFF     ; was 2D 31 (adds r1,#0x2D)
```

### Change 2 – hook at `0x356C` (broken)

`bl 0x4A7C` at `0x356C` is redirected to `bl 0x7000`. The stub at `0x7000` is:

```
7000: FD F7 3C FD   bl   0x4A7C
7004: 00 21         movs r1,#0
7006: 04 20         movs r0,#4
7008: FC F7 28 FA   bl   0x345C
700C: 00 21         movs r1,#0
700E: 68 20         movs r0,#0x68
7010: FF F7 D0 FB   bl   0x67B4
7014: 70 47         bx   lr          ; lr = 0x7015 here
```

**Bug: `lr` is never saved.** Each `bl` overwrites `lr`, so the final `bx lr` jumps to `0x7014` itself and loops forever. Interrupts keep running, but the main loop no longer feeds the watchdog, so the MCU almost certainly resets at the timeout instead of stopping cleanly. This is inferred from the code and was not observed on the device.

It also reports DP `0x68` (play mode) = 0. Reporting DP `0x69` = false would show the switch as off in the app.

## Corrected stub

Assembled with `arm-none-eabi-as -mcpu=cortex-m0`.

**Status (2026-10-30):** the owner flashed a working stub at `0x7000` manually, together with the `0x3548` and `0x356C` changes.

The flashed variant keeps the original design choice: it reports **DP `0x68` (104) = 0**, not DP `0x69`. That value serves as a "play timer finished" marker. Neither the app nor the bridge ever sends mode 0, so it can be told apart from a user's off command. The `lasercat` bridge shows the light as off when it receives it.

The exact bytes on the device were not read back. A working variant must save `lr` as above; the `flash_600.bin` version without `push {lr}` would hang.

DP 104 = 0 would also be reported if someone sent mode 0 to DP `0x68`: the firmware then runs the auto pattern with the laser on. The bridge never sends that.

```
7000: 00 B5           push {lr}
7002: FD F7 3B FD     bl   0x4A7C      ; original call (step size = 1)
7006: 00 21           movs r1,#0
7008: 04 20           movs r0,#4
700A: FC F7 27 FA     bl   0x345C      ; mode = 4, g_02E = 0
700E: 00 21           movs r1,#0
7010: 69 20           movs r0,#0x69
7012: FF F7 67 FB     bl   0x66E4      ; report DP 0x69 (bool) = false
7016: 00 BD           pop  {pc}
```

The hook at `0x356C` (`03 F0 48 FD` = `bl 0x7000`) and the `0x3548` change stay as they are.

Byte string for `0x7000`:

```
00 B5 FD F7 3B FD 00 21 04 20 FC F7 27 FA 00 21 69 20 FF F7 67 FB 00 BD
```

### Notes

- Resetting `g_02E` in the stub means the stop branch fires again every 510 s while stopped. Each time it re-sends the off report and centres the servos again, which is harmless.
- `0x66E4` returns early without sending when `g_05C == 1`, the same as every other report.
