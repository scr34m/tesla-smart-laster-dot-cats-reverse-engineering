# Firmware analysis notes

Static analysis of `flash.bin` (HC32L110C6PA, Cortex-M0+, Thumb, 32 KB flash at `0x00000000`).

- [firmware-overview.md](firmware-overview.md) – memory layout, boot flow, pins, main loop, play modes
- [bootloader.md](bootloader.md) – start-up decision, Tuya OTA receiver, CRC check, flag handling
- [ble-ota.md](ble-ota.md) – updating the MCU over Bluetooth through the Tuya module (`mcu_ota/ble_ota.py`)
- [tuya-protocol.md](tuya-protocol.md) – UART framing, handled commands, data points, OTA hand-off
- [ram-map.md](ram-map.md) – recovered meaning of RAM variables and the motion state struct
- [timeout-patch.md](timeout-patch.md) – the 5-minute auto-off logic and a corrected patch stub

## Method

- Decompiled with REA (`rea decompile-all`, Ghidra 12.1.3 provider) as a raw ARM image at entry/load address `0x0`:
  ```
  GHIDRA_INSTALL_DIR=/opt/homebrew/opt/ghidra/libexec \
  JAVA_HOME=/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home \
  rea decompile-all flash.bin --output decomp --target-format raw-binary \
      --architecture arm --entry-point 0x0 --provider ghidra
  ```
  Result: 240 functions, SHA-256 of the input `8a763dd579b0218626179d451414a7eeda4d168b6a16763a5ff1cb2016d39699`.
- Literal-pool pointers (`DAT_xxxx`) were resolved to RAM variables (`g_XXX` = `0x20000XXX`) and to peripheral registers using `HC32L110C.svd`.
  The SVD puts several peripherals on the same base (`0x40000C00` = BT0/LPTIMER/WDT, `0x40002400` = ADC/BGR/LVD/VC), so names there are ambiguous.
- Ghidra cannot follow the Keil `switch8` helper at `0x4B9E` (case count byte at `LR-1`, offset table after it, `LR` carries the Thumb bit). The Tuya dispatchers at `0x5A60`, `0x5EE8` and `0x6840` were therefore read from
  `arm-none-eabi-objdump -D -b binary -m arm -M force-thumb flash.bin`.
- Everything here is static analysis. Timing values assume the base-timer-2 interrupt fires every 1 ms, which is consistent with the observed 5-minute stop but was not measured.

## Address conventions

- Code addresses are flash offsets (the README's `FUN_xxxxxxxx` names from Ghidra map 1:1).
- `g_XXX` means the RAM byte/halfword/word at `0x20000XXX`.
