# Bootloader (`0x0000–0x1FFF`)

The flash holds two separate programs:

- **Bootloader** (`0x0000–0x1FFF`). Its only job is to choose between starting the application and receiving a new one over UART.
- **Application** (`0x2000` onward).

The bootloader does not wait for any data before starting the application. The choice depends on a single flash byte.

## Start-up decision (`main` at `0x1174`)

```
init clocks, UART, flash timing
if (*(u8 *)0x7E00 == 0x55)          // update flag
    enter_update_mode();            // 0x1924
else
    jump_to_app(0x2000);            // 0x1144
for (;;) uart_poll();               // 0x0E04 – Tuya frame parser
```

`jump_to_app` (`0x1144`):

1. Reads the first word at `0x2000`, the application's initial SP.
2. Checks it with `(sp & 0x2FFE0000) == 0x20000000`, i.e. that it points into RAM.
3. If so, sets MSP and branches to the reset vector at `0x2004`.

**No CRC or signature is checked on a normal boot.** If the SP check fails, `jump_to_app` returns and the bootloader stays in the UART loop, waiting for an update.

There is no timeout. With the flag set, or with an invalid application, the device stays in the bootloader until an update succeeds, even across power cycles.

## Entering update mode

The application triggers update mode:

1. The Tuya module sends cmd `0xEA` to the application. Handler: application `0x6840`.
2. The application erases the `0x7E00` sector and writes `0x55`, then `"gcslbvj0"` and the version bytes.
3. It sets `g_049 = 1`, so `main` stops feeding the watchdog.
4. The watchdog resets the MCU.
5. The bootloader finds `0x55` at `0x7E00`.
6. `enter_update_mode` (`0x1924`) itself sends an `0xEA` reply: version bytes, then `0x00 0x80`, meaning a 128-byte packet size. It then waits for the transfer.

## UART framing in the bootloader

- **Link:** UART0 on P1.4/P1.5, 9600 baud 8N1 (`0x0CBA`: baud = `75 << 7`).
- **Frame:** `55 AA 00 cmd lenH lenL data cksum`. The parser (`0x11B0`) syncs on `55 AA 00`, so the version byte must be `0x00`. The maximum frame is 0x133 bytes.
- **Checksum:** the trailing byte is **not verified**. Replies are sent with a correct checksum (`0x0EA8`).
- **Other commands** (`0x0F3C`): the bootloader also answers `0x00` heartbeat, `0x01` product info, `0x02` work mode and `0xE8` version.
  - `0x03` network status with value 0 or 1 **resets the OTA transfer state** (`0x179C`).

### Payloads

The integers below are big-endian.

| cmd | Request | Reply |
|---|---|---|
| `0xEA` | – | `00, ver[3], 00, 80` (installed version from `0x7E09`, packet size 128). Only sent in state 0; `0x1924` sends it once by itself at boot |
| `0xEB` | `PID[8], ver[3], md5[16], size[4], crc32[4]` | 25 bytes, first byte status: 0 ok, 1 PID ≠ `0x7E01..08`, 2 version not greater than `0x7E09..0B`, 3 size > `0x5FFF` |
| `0xEC` | `offset[4]` (0 = restart) | 4 bytes describing the resume point |
| `0xED` | `seq[2], len[2], 2 unused bytes, data[len ≤ 128]` | status: 0 ok, 1 sequence, 4 flash/address, 5 too long |
| `0xEE` | – | status: 0 ok (then jumps to the app), 1 size mismatch, 2 CRC mismatch |

Packet-size rule: a 512-byte page is erased only when a packet starts exactly on a page boundary. Packet sizes must therefore divide 512.

CRC is standard CRC-32 (zlib: init and final XOR `0xFFFFFFFF`, polynomial `0xEDB88320`) over the image bytes.

The uploader in [../mcu_ota/](../mcu_ota/) implements this sequence.

## OTA command handling

The dispatcher at `0x1874` switches on `cmd − 0xEA`. It uses the `switch8` helper at `0x0D30` with table bytes `05 | 04 09 0E 13 18 | 1D`. Each handler replies with its own command number. Progress is tracked in a state variable (`g_040`: 0 idle … 4 receiving).

| cmd | Handler | Function |
|---|---|---|
| `0xEA` | `0x18C8` | Upgrade request / start |
| `0xEB` | `0x1540` | File info: checks the 8-byte product ID and the version, file size ≤ `0x5FFF` (fits `0x2000–0x7FFF`) and the expected CRC32. If it is the same file as a saved interrupted transfer, the transfer resumes |
| `0xEC` | `0x17D4` | File offset: offset 0 restarts from scratch, otherwise replies with how much has already been received (resume support) |
| `0xED` | `0x13C4` | Data packet: checks the sequence number (previous + 1, or 0) and a length ≤ 128. Erases each 512-byte page before its first write, writes to `0x2000 + offset`, updates the running CRC32 (`0x0EEC`, polynomial `0xEDB88320`). Saves the transfer state to the `0x7C00` sector every 32 packets |
| `0xEE` | `0x1524` → `0x1A04` | End: requires received bytes == file size. Re-computes CRC32 over the flash from `0x2000` (`0x1040`) and compares it with the expected value |

### End of transfer (`0x1A04`)

- **CRC matches**: replies `0xEE` with status 0, saves the state to `0x7C00`, and **jumps straight to the new application** (`0x1ADC` → `0x1144(0x2000)`).
- **Mismatch / size error**: replies with an error status, clears the saved transfer state, and stays in the bootloader for another attempt.

Any packet error (wrong sequence, flash write failure, oversized packet) also resets the transfer state.

## Who clears the flag

The bootloader never clears the `0x55` at `0x7E00`, even after a successful update. The application does: every 100 main-loop iterations, `0x3680` erases the `0x7E00` sector if it still starts with `0x55`. So once the new application has run, the next reset boots it directly.

## Flash sectors used by the bootloader

| Address | Content |
|---|---|
| `0x2000–0x7FFF` | Application image being written |
| `0x7C00` | OTA transfer state (0x178 bytes from `g_324`: received size, running CRC, expected size/CRC, last sequence number), so an interrupted transfer can resume |
| `0x7E00` | Update flag written by the application |

The OTA image can be up to `0x5FFF` bytes, which reaches into `0x7A00` (application settings) and `0x7C00`. This firmware's application ends below `0x7000`.

## Consequences for patching

- Since a normal boot checks no CRC, an application patched over SWD (`flash_600.bin`) starts without problems.
- A later Tuya OTA writes a complete new image from `0x2000`, which overwrites any SWD patch, including a stub at `0x7000` if the new image is that large.
- The bootloader only loads MSP and jumps. It does not set `VTOR` (the original app sets `SCB->VTOR = 0x2000` itself at `0x20E2`) and does not clear the `0x7E00` flag.
  A custom application must do both; see [../mcu_ota/README.md](../mcu_ota/README.md#requirements-for-your-own-firmware).
