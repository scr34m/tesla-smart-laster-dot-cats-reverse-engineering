# MCU firmware update

This folder has two uploaders for the same bootloader protocol:

- **`ble_ota.py`**: the normal wireless path through the Tuya BLE module, see [../docs/ble-ota.md](../docs/ble-ota.md). It needs the external `ble_tuya_client` repository next to this folder (for its `tuya_ble` package).
- **`uart_ota.py`**, described below: the wired fallback when the BLE module can't be used.

## UART uploader

`uart_ota.py` sends an application image to the device's own bootloader over the MCU UART, standing in for the Tuya BLE module. It writes flash `0x2000` onward; the bootloader (`0x0000–0x1FFF`) and the settings sector at `0x7A00` are left untouched.

The protocol was reconstructed from the flash dump, see [../docs/bootloader.md](../docs/bootloader.md).

The tool has been checked against a simulator built from the disassembly, not yet against real hardware.

## Wiring

| USB-UART | MCU pin |
|---|---|
| TX | 11 (P1.5, UART0 RX) |
| RX | 12 (P1.4, UART0 TX) |
| GND | 7 |

The Tuya BLE module is wired to the same two pins. Disconnect its TX line (or hold the module in reset) while uploading, otherwise both drive the MCU's RX pin.

Settings: 3.3 V logic, 9600 baud, 8N1.

## Usage

```
pip install -r requirements.txt

# Device still runs the original firmware in Tuya mode:
python uart_ota.py -p /dev/tty.usbserial-0001 --trigger my_app.bin

# Device is already in the bootloader (flag 0x55 at 0x7E00): start the tool, then power-cycle the device
python uart_ota.py -p /dev/tty.usbserial-0001 my_app.bin

# Restore the original application from the dump
python uart_ota.py -p /dev/tty.usbserial-0001 --from-dump ../flash.bin
```

| Option | Meaning |
|---|---|
| `--trigger` | Sends `0xEA` to the running original application. It writes the update flag, stops feeding the watchdog and resets into the bootloader (takes a few seconds). |
| `--pid` | Product ID the bootloader compares against `0x7E01..0x7E08`. Default `gcslbvj0`, which is what the original application writes. |
| `--version X.Y.Z` | Version announced in the file info. It must be **strictly greater** than the one stored at `0x7E09..0x7E0B`. Default: installed version + 1, taken from the bootloader's `0xEA` message. |
| `--from-dump` | Cut `0x2000..0x79FF` out of a 32 KB dump and strip trailing `0xFF`. |
| `--allow-settings-overwrite` | Allow images up to `0x5C00` bytes, which overwrites the settings at `0x7A00`. The OTA state sector at `0x7C00` is always protected. |
| `-v` | Print the raw frames. |

Without `--trigger`, the tool sends `0xEA` every second while waiting.

- If the bootloader is idle, it answers that `0xEA` directly.
- If it has just booted, it has already announced itself with its own `0xEA`, so power-cycling the device while the tool is waiting also works.

## Sequence

```
bootloader → EA  00 vv vv vv 00 80          ready, installed version, 128-byte packets
tool       → EB  PID(8) ver(3) md5(16) size(4) crc32(4)
bootloader → EB  00 ...                     0 = accepted
tool       → EC  00 00 00 00                start at offset 0
bootloader → EC  ...
tool       → ED  seq(2) len(2) 00 00 data(128)     repeated, seq = 0, 1, 2 ...
bootloader → ED  00
tool       → EE
bootloader → EE  00                         size + CRC32 OK → jumps to 0x2000
```

On any error the bootloader resets its transfer state. Run the tool again; it starts over from `0xEA`.

## Requirements for your own firmware

The bootloader starts anything whose first word (initial SP) points into RAM. It does not set up the vector table or clear the update flag, so your application has to do the following.

1. **Link it for `0x2000`.** Put the vector table at the start of the image: `FLASH ORIGIN = 0x00002000, LENGTH = 0x5A00` (stays below the settings sector at `0x7A00`; use `0x5C00` if you don't need it). The output is a raw `.bin` starting at `0x2000`.
2. **Set `SCB->VTOR = 0x2000`** first thing in the reset handler. The original app does this at `0x20E2`; the bootloader only loads MSP and jumps.
3. **Clear the update flag.** If the byte at `0x7E00` is `0x55`, erase that 512-byte sector early after boot.
   - The bootloader never clears it.
   - If it stays set, every reset ends up in the bootloader, which waits forever.
   - That is recoverable with this tool or SWD, but the device appears dead until then.
4. **Optional: support the next OTA without SWD.** On a command of your choice (e.g. Tuya `0xEA` like the original), do this:
   1. Erase the `0x7E00` sector.
   2. Write `0x55`, then the 8-byte PID, then a 3-byte version.
   3. Stop feeding the watchdog, or call `NVIC_SystemReset()`.

   Keep the version low (e.g. `0.0.1`), because each upload must announce a higher one.

   If the sector ends up as `0x55` followed by `0xFF`s, the stored version is `255.255.255` and the bootloader will reject every image. In that case write the sector again via SWD.

The image size limit is `0x5FFF` bytes on the bootloader side. The tool stops at `0x5A00` to keep the settings sector and the bootloader's own state sector at `0x7C00` intact.
