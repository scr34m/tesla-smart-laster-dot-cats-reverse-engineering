#!/usr/bin/env python3
"""
Update the laser-cat MCU firmware over Bluetooth.

The Tuya BLE module forwards OTA packets whose first payload byte (type) is 1
to the MCU as the serial commands 0xEA-0xEE, so each BLE payload here is
0x01 + the serial payload that the HC32L110 bootloader expects
(see ../docs/bootloader.md and ../docs/ble-ota.md).

The BLE link and Tuya session come from the unmodified tuya_ble package of
../ble_tuya_client; only the OTA reply handling is added here by subclassing.
"""

import argparse
import asyncio
import hashlib
import logging
import os
import struct
import sys
import time
import zlib
from pathlib import Path

from bleak import BleakScanner

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "ble_tuya_client"))
from tuya_ble import (  # noqa: E402
    AbstaractTuyaBLEDeviceManager, TuyaBLEDevice, TuyaBLEDeviceCredentials,
)
from tuya_ble.const import TuyaBLECode  # noqa: E402

from uart_ota import (  # noqa: E402
    APP_BASE, DATA_STATUS, END_STATUS, FILE_INFO_STATUS, OTA_STATE_ADDR,
    PACKET_SIZE, SETTINGS_ADDR, OtaError, fmt_version, load_image, parse_version,
)

OTA_TYPE_MCU = 0x01
OTA_CODES = {
    TuyaBLECode.FUN_SENDER_OTA_START,
    TuyaBLECode.FUN_SENDER_OTA_FILE,
    TuyaBLECode.FUN_SENDER_OTA_OFFSET,
    TuyaBLECode.FUN_SENDER_OTA_UPGRADE,
    TuyaBLECode.FUN_SENDER_OTA_OVER,
}


class Credentials(AbstaractTuyaBLEDeviceManager):
    def __init__(self, uuid, local_key, device_id):
        self.credentials = TuyaBLEDeviceCredentials(
            uuid, local_key, device_id, "", "", None, None, None)

    async def get_device_credentials(self, address, force_update=False, save_data=False):
        return self.credentials


class OtaDevice(TuyaBLEDevice):
    """TuyaBLEDevice that also returns the payload of OTA replies."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._ota_responses: dict[int, asyncio.Future[bytes]] = {}

    def _handle_command_or_response(self, seq_num, response_to, code, data):
        if code in OTA_CODES:
            future = self._ota_responses.pop(code.value, None)
            if future and not future.done():
                future.set_result(data)
        super()._handle_command_or_response(seq_num, response_to, code, data)

    async def send_ota_command(self, code, data, timeout):
        future = asyncio.get_running_loop().create_future()
        self._ota_responses[code.value] = future
        try:
            await self._send_packet(code, data, wait_for_response=False)
            return await asyncio.wait_for(future, timeout)
        finally:
            self._ota_responses.pop(code.value, None)


def strip_type(reply, serial_len):
    """The module may echo the type byte in front of the MCU's reply."""
    if len(reply) == serial_len + 1 and reply[0] in (0x00, OTA_TYPE_MCU):
        return reply[1:]
    return reply


class BleMcuOta:
    def __init__(self, device):
        self.device = device

    async def command(self, code, payload, reply_len, timeout):
        reply = await self.device.send_ota_command(
            code, bytes([OTA_TYPE_MCU]) + payload, timeout)
        reply = strip_type(reply, reply_len)
        if len(reply) < reply_len:
            raise OtaError(f"{code.name}: short reply {reply.hex(' ')}")
        return reply

    async def start(self, timeout):
        # The original application answers 0xEA by writing the update flag and
        # letting the watchdog reset it; the bootloader then sends the reply.
        print(f"Requesting MCU update mode, waiting up to {timeout:.0f} s for the bootloader ...")
        try:
            reply = await self.command(TuyaBLECode.FUN_SENDER_OTA_START,
                                       struct.pack(">H", PACKET_SIZE), 6, timeout)
        except asyncio.TimeoutError:
            print("No 0xEA reply. If the bootloader was already waiting it does not answer twice; "
                  "continuing with the file info.")
            return None, PACKET_SIZE
        if reply[0] != 0:
            raise OtaError("MCU rejected the update request")
        current = (reply[1] << 16) | (reply[2] << 8) | reply[3]
        packet = min(struct.unpack(">H", reply[4:6])[0] or PACKET_SIZE, PACKET_SIZE)
        if 512 % packet:
            raise OtaError(f"unexpected packet size {packet}")
        print(f"Bootloader ready: installed version {fmt_version(current)}, packet size {packet}")
        return current, packet

    async def upload(self, image, pid, version, current, packet):
        size = len(image)
        crc = zlib.crc32(image) & 0xFFFFFFFF
        if version is None:
            if current is None:
                raise OtaError("installed version unknown, pass --version")
            if current >= 0xFFFFFF:
                raise OtaError("installed version is 255.255.255; nothing can be newer")
            version = current + 1
        print(f"Image: {size} bytes ({size:#x}), CRC32 {crc:08X}, "
              f"version {fmt_version(version)}, PID {pid.decode()}")

        info = (pid + struct.pack(">I", version)[1:] + hashlib.md5(image).digest()
                + struct.pack(">II", size, crc))
        reply = await self.command(TuyaBLECode.FUN_SENDER_OTA_FILE, info, 25, 10)
        if reply[0] != 0:
            raise OtaError(f"file info rejected: {FILE_INFO_STATUS.get(reply[0], reply[0])}")

        await self.command(TuyaBLECode.FUN_SENDER_OTA_OFFSET, struct.pack(">I", 0), 4, 10)

        started = time.monotonic()
        count = (size + packet - 1) // packet
        for seq in range(count):
            chunk = image[seq * packet:(seq + 1) * packet]
            crc16 = TuyaBLEDevice._calc_crc16(chunk)
            reply = await self.command(TuyaBLECode.FUN_SENDER_OTA_UPGRADE,
                                       struct.pack(">HHH", seq, len(chunk), crc16) + chunk, 1, 15)
            if reply[0] != 0:
                raise OtaError(f"packet {seq} rejected: {DATA_STATUS.get(reply[0], reply[0])}")
            done = min((seq + 1) * packet, size)
            print(f"\r  {done:6d}/{size} bytes  {100 * done // size:3d}%", end="", flush=True)
        print(f"\n  transferred in {time.monotonic() - started:.1f} s")

        reply = await self.command(TuyaBLECode.FUN_SENDER_OTA_OVER, b"", 1, 20)
        if reply[0] != 0:
            raise OtaError(f"verification failed: {END_STATUS.get(reply[0], reply[0])}")
        print("Verified. The bootloader is now starting the new application.")


async def find_device(address, timeout):
    found = asyncio.get_running_loop().create_future()

    def on_detect(device, advertisement_data):
        if device.address.lower() == address.lower() and not found.done():
            found.set_result((device, advertisement_data))

    print(f"Scanning for {address} ...")
    async with BleakScanner(on_detect):
        return await asyncio.wait_for(found, timeout)


async def run(args):
    pid = args.pid.encode()
    end = OTA_STATE_ADDR if args.allow_settings_overwrite else SETTINGS_ADDR
    image = load_image(args.image, args.from_dump, end - APP_BASE)

    ble_device, advertisement_data = await find_device(args.addr, 20)
    device = OtaDevice(Credentials(args.uuid, args.key, args.devid), ble_device, advertisement_data)
    await device.initialize()
    await device._ensure_connected()
    if not device._is_paired:
        raise OtaError("could not pair with the device (check uuid / local key / device id)")
    print("Connected and paired.")

    ota = BleMcuOta(device)
    try:
        current, packet = await ota.start(args.wait)
        await ota.upload(image, pid, args.version, current, packet)
    finally:
        await device.stop()


def main():
    env = os.environ.get
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("image", help="raw binary linked for 0x2000 (or a 32 KB dump with --from-dump)")
    ap.add_argument("--addr", default=env("LASERCAT_ADDR"),
                    help="BLE address / CoreBluetooth UUID (env LASERCAT_ADDR)")
    ap.add_argument("--uuid", default=env("LASERCAT_UUID"), help="Tuya device uuid (env LASERCAT_UUID)")
    ap.add_argument("--key", default=env("LASERCAT_KEY"), help="Tuya local key (env LASERCAT_KEY)")
    ap.add_argument("--devid", default=env("LASERCAT_DEVID"), help="Tuya device id (env LASERCAT_DEVID)")
    ap.add_argument("--pid", default="gcslbvj0", help="8-character product ID (default: %(default)s)")
    ap.add_argument("--version", type=parse_version,
                    help="version to announce as X.Y.Z (default: installed version + 1)")
    ap.add_argument("--from-dump", action="store_true",
                    help="take the application part (0x2000..0x79FF) out of a full flash dump")
    ap.add_argument("--allow-settings-overwrite", action="store_true",
                    help="allow images that reach into the settings sector at 0x7A00")
    ap.add_argument("--wait", type=float, default=30.0,
                    help="seconds to wait for the bootloader after the request (default: %(default)s)")
    ap.add_argument("-v", "--verbose", action="store_true", help="debug logging of the BLE traffic")
    args = ap.parse_args()

    missing = [n for n in ("addr", "uuid", "key", "devid") if not getattr(args, n)]
    if missing:
        ap.error("missing " + ", ".join("--" + n for n in missing) + " (or the LASERCAT_* variables)")
    if len(args.pid.encode()) != 8:
        ap.error("--pid must be exactly 8 characters")
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.WARNING)
    logging.getLogger("bleak").setLevel(logging.WARNING)

    try:
        asyncio.run(run(args))
    except (OtaError, OSError, asyncio.TimeoutError) as exc:
        print(f"\nError: {exc or type(exc).__name__}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
