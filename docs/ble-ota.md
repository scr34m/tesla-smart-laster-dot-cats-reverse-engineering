# MCU update over Bluetooth

The normal update path needs no wire: the phone (or `mcu_ota/ble_ota.py`) sends the image to the Tuya BLE module, and the module relays it to the MCU over the serial link with the commands `0xEA–0xEE` described in [bootloader.md](bootloader.md).

## How the module relays

Two sources describe the relay:

- **Tuya BLE SDK documentation:** `tuya_ble_handle_ota_req` routes an OTA packet to the MCU when the first payload byte (the OTA *type*) is `1`. It calls `tuya_ble_uart_common_mcu_ota_data_from_ble_handler(cmd, &recv_data[14], data_len-1)`, i.e. it drops that type byte and forwards the rest unchanged. Replies from the MCU (`0xEA–0xEE`) go back to the app through `tuya_ble_uart_common_mcu_ota_data_from_uart_handler`.
- **Tuya serial OTA specification:** the "Bluetooth LE MCU OTA Service" payloads match this bootloader byte for byte. The two bytes before the data in `0xED` are a CRC16 of the packet, which this bootloader does not check.

| BLE command | Code | BLE payload | Serial command to MCU |
|---|---|---|---|
| OTA start | `0x000C` | `01` + max packet size (2) | `0xEA` |
| File info | `0x000D` | `01` + PID(8) ver(3) md5(16) size(4) crc32(4) | `0xEB` |
| File offset | `0x000E` | `01` + offset(4) | `0xEC` |
| Data | `0x000F` | `01` + seq(2) len(2) crc16(2) data | `0xED` |
| End | `0x0010` | `01` | `0xEE` |

The module's own firmware is not part of this dump. The relay rules above come from Tuya's documentation, not from code observed here.

## Sequence with the original firmware

1. The client connects and pairs (local key, as for DP control).
2. OTA start reaches the **application** as `0xEA`.
   - It writes `0x55`, `"gcslbvj0"` and its version to `0x7E00`, then stops feeding the watchdog. It sends no reply itself.
3. The watchdog resets the MCU and the bootloader starts. It sends `0xEA [00, version, 00 80]`, which the module relays back as the answer.
   - While waiting, the bootloader answers the module's heartbeat (`0x00`) and product info (`0x01`) with the same PID and version taken from `0x7E00`, so the module keeps seeing the same product.
4. File info, offset, 128-byte data packets and end follow, exactly as on the serial link.
5. After a correct CRC32 the bootloader jumps straight into the new application.

The bootloader resets its transfer state if it receives network status `0x03` with value 0 or 1 (module unbound/disconnected). The BLE connection therefore has to stay up for the whole transfer. If it drops, run the tool again: the bootloader keeps waiting, because the `0x55` flag is still set.

## Tool

```
cd mcu_ota
export LASERCAT_ADDR=… LASERCAT_UUID=… LASERCAT_KEY='…' LASERCAT_DEVID=…   # same as for lasercat
python ble_ota.py my_app.bin                  # own image linked for 0x2000
python ble_ota.py --from-dump ../flash.bin    # put the original application back
```

[../mcu_ota/ble_ota.py](../mcu_ota/ble_ota.py) uses the `tuya_ble` package of the external `ble_tuya_client` repository (expected next to `mcu_ota/`) without modifying it. The OTA reply handling is added in a `TuyaBLEDevice` subclass. Credentials come from `--addr/--uuid/--key/--devid` or the `LASERCAT_*` variables, not from `device_manager.py`.

The image checks, the version rule (must be newer than the one in `0x7E09`) and the 0x5A00 size limit are shared with [../mcu_ota/uart_ota.py](../mcu_ota/uart_ota.py).
- The protocol logic was checked against a simulated module and bootloader. It has **not yet run against the real BT3L module**.

### Open points to watch on the first real run

- **Echoed type byte:** whether the module echoes the type byte in its replies. The tool accepts both forms.
- **CRC16:** whether the module verifies the per-packet CRC16 before forwarding. The tool sends CRC-16/MODBUS (`0xA001`, init `0xFFFF`), the same CRC the Tuya BLE link layer uses.
- **Reply timing:** whether the module waits long enough for the `0xEA` reply. The reply only comes after the watchdog reset, a few seconds later. If it times out, the tool continues with file info anyway, because the bootloader is then already waiting.

Run with `-v` to log the BLE traffic.

The serial fallback ([../mcu_ota/](../mcu_ota/)) works without the module, for example when the module is unbound or the MCU application no longer handles `0xEA`.

## Sources

- [Bluetooth LE MCU OTA Service – serial payloads](https://developer.tuya.com/en/docs/mcu-standard-protocol/Bluetooth-LE-MCU-OTA-Service?id=Kd3wc0s33242s)
- [TuyaOS BLE OTA – BLE payload layouts](https://developer.tuya.com/en/docs/iot-device-dev/OTA_BLE?id=Kawer1ehuv8si)
- [Update Firmware – MCU routing in `tuya_ble_handle_ota_req`](https://developer.tuya.com/en/docs/iot/Firmware-ota-upgrade?id=Kbfkbc8j3er6y)
