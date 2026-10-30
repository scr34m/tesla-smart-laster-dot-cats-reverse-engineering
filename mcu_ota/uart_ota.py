#!/usr/bin/env python3
"""
Upload an application image to the HC32L110 laser-cat bootloader over UART.

The bootloader (flash 0x0000-0x1FFF) speaks the Tuya MCU OTA commands
0xEA-0xEE at 9600 8N1 and writes the image to 0x2000. This script plays the
role of the Tuya module. Protocol details: ../docs/bootloader.md
"""

import argparse
import hashlib
import struct
import sys
import time
import zlib

import serial

APP_BASE = 0x2000
SETTINGS_ADDR = 0x7A00      # application settings sector (centre, timers)
OTA_STATE_ADDR = 0x7C00     # bootloader resume state, must never be overwritten
PACKET_SIZE = 128           # announced by the bootloader; must divide the 512 B page

CMD_UPGRADE_START = 0xEA
CMD_FILE_INFO = 0xEB
CMD_FILE_OFFSET = 0xEC
CMD_DATA = 0xED
CMD_END = 0xEE

FILE_INFO_STATUS = {
    0: "ok",
    1: "product ID mismatch",
    2: "version is not newer than the installed one",
    3: "image too large",
}
DATA_STATUS = {
    0: "ok",
    1: "packet sequence error",
    4: "flash write error / address out of range",
    5: "packet too long",
}
END_STATUS = {
    0: "ok",
    1: "received size does not match file size",
    2: "CRC32 mismatch",
}


class OtaError(Exception):
    pass


def build_frame(cmd, data=b""):
    frame = bytes([0x55, 0xAA, 0x00, cmd]) + struct.pack(">H", len(data)) + data
    return frame + bytes([sum(frame) & 0xFF])


class Link:
    def __init__(self, port, verbose=False):
        self.port = port
        self.verbose = verbose
        self.buf = bytearray()

    def send(self, cmd, data=b""):
        frame = build_frame(cmd, data)
        if self.verbose:
            print(f"  -> {frame[:16].hex(' ')}{' ...' if len(frame) > 16 else ''}")
        self.port.write(frame)
        self.port.flush()

    def _next_frame(self):
        while True:
            start = self.buf.find(b"\x55\xAA")
            if start < 0:
                del self.buf[:-1]
                return None
            del self.buf[:start]
            if len(self.buf) < 6:
                return None
            length = struct.unpack_from(">H", self.buf, 4)[0]
            if len(self.buf) < 7 + length:
                return None
            frame = bytes(self.buf[:7 + length])
            if sum(frame[:-1]) & 0xFF != frame[-1]:
                del self.buf[:2]            # bad checksum, resync
                continue
            del self.buf[:7 + length]
            if self.verbose:
                print(f"  <- {frame[:16].hex(' ')}{' ...' if len(frame) > 16 else ''}")
            return frame[3], frame[6:-1]

    def wait_for(self, cmd, timeout):
        """Return the payload of the next frame with the given command, or None."""
        deadline = time.monotonic() + timeout
        while True:
            frame = self._next_frame()
            while frame is not None:
                if frame[0] == cmd:
                    return frame[1]
                frame = self._next_frame()  # heartbeats etc. are ignored
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            self.port.timeout = min(remaining, 0.1)
            self.buf += self.port.read(max(1, self.port.in_waiting))


def load_image(path, from_dump, max_size):
    data = open(path, "rb").read()
    if from_dump:
        if len(data) != 0x8000:
            raise OtaError(f"--from-dump expects a 32 KB flash dump, got {len(data)} bytes")
        data = data[APP_BASE:SETTINGS_ADDR].rstrip(b"\xFF")
    if not data:
        raise OtaError("image is empty")
    if len(data) > max_size:
        raise OtaError(f"image is {len(data):#x} bytes, limit is {max_size:#x} "
                       f"(0x{APP_BASE:04X}..0x{APP_BASE + max_size:04X})")

    sp, reset = struct.unpack_from("<II", data, 0)
    if sp & 0x2FFE0000 != 0x20000000:
        raise OtaError(f"initial SP {sp:#010x} is not in RAM; the bootloader would not start it")
    if not reset & 1 or not APP_BASE <= (reset & ~1) < APP_BASE + len(data):
        raise OtaError(f"reset vector {reset:#010x} is not a Thumb address inside the image; "
                       f"is it linked for 0x{APP_BASE:04X}?")
    return data


def parse_version(text):
    parts = [int(p, 0) for p in text.split(".")]
    if len(parts) != 3 or not all(0 <= p <= 255 for p in parts):
        raise argparse.ArgumentTypeError("version must be X.Y.Z with 0..255 each")
    return (parts[0] << 16) | (parts[1] << 8) | parts[2]


def fmt_version(v):
    return f"{v >> 16}.{(v >> 8) & 0xFF}.{v & 0xFF}"


def wait_for_bootloader(link, trigger, timeout):
    if trigger:
        print("Asking the running application to enter the bootloader (0xEA) ...")
        link.send(CMD_UPGRADE_START)

    print(f"Waiting up to {timeout:.0f} s for the bootloader "
          "(power-cycle the device now if it is not starting) ...")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        # The bootloader announces itself with an unsolicited 0xEA right after
        # reset; while idle it also answers our 0xEA.
        reply = link.wait_for(CMD_UPGRADE_START, 1.0)
        if reply is not None and len(reply) >= 6:
            current = (reply[1] << 16) | (reply[2] << 8) | reply[3]
            packet = (reply[4] << 8) | reply[5]
            print(f"Bootloader ready: installed version {fmt_version(current)}, "
                  f"packet size {packet}")
            if packet and 512 % min(packet, PACKET_SIZE):
                raise OtaError(f"unexpected packet size {packet}")
            return current
        if not trigger:
            link.send(CMD_UPGRADE_START)
    raise OtaError("no answer from the bootloader")


def upload(link, image, pid, version, current):
    size = len(image)
    crc = zlib.crc32(image) & 0xFFFFFFFF
    md5 = hashlib.md5(image).digest()
    if version is None:
        if current >= 0xFFFFFF:
            raise OtaError("installed version is 255.255.255; nothing can be newer. "
                           "Rewrite the flag sector at 0x7E00 with a lower version.")
        version = current + 1
    print(f"Image: {size} bytes ({size:#x}), CRC32 {crc:08X}, "
          f"version {fmt_version(version)}, PID {pid.decode()}")

    info = pid + struct.pack(">I", version)[1:] + md5 + struct.pack(">II", size, crc)
    link.send(CMD_FILE_INFO, info)
    reply = link.wait_for(CMD_FILE_INFO, 3.0)
    if reply is None:
        raise OtaError("no reply to file info (0xEB); the bootloader may be mid-transfer, "
                       "power-cycle it and retry")
    if reply[0] != 0:
        raise OtaError(f"file info rejected: {FILE_INFO_STATUS.get(reply[0], reply[0])}")

    link.send(CMD_FILE_OFFSET, struct.pack(">I", 0))
    if link.wait_for(CMD_FILE_OFFSET, 3.0) is None:
        raise OtaError("no reply to file offset (0xEC)")

    started = time.monotonic()
    packets = (size + PACKET_SIZE - 1) // PACKET_SIZE
    for seq in range(packets):
        chunk = image[seq * PACKET_SIZE:(seq + 1) * PACKET_SIZE]
        link.send(CMD_DATA, struct.pack(">HHH", seq, len(chunk), 0) + chunk)
        # Every 512 bytes a page is erased, every 32 packets the resume state is saved.
        reply = link.wait_for(CMD_DATA, 5.0)
        if reply is None:
            raise OtaError(f"no reply to packet {seq}")
        if reply[0] != 0:
            raise OtaError(f"packet {seq} rejected: {DATA_STATUS.get(reply[0], reply[0])}")
        done = min((seq + 1) * PACKET_SIZE, size)
        print(f"\r  {done:6d}/{size} bytes  {100 * done // size:3d}%", end="", flush=True)
    print(f"\n  transferred in {time.monotonic() - started:.1f} s")

    link.send(CMD_END)
    reply = link.wait_for(CMD_END, 10.0)    # the bootloader re-reads the flash for the CRC
    if reply is None:
        raise OtaError("no reply to end (0xEE)")
    if reply[0] != 0:
        raise OtaError(f"verification failed: {END_STATUS.get(reply[0], reply[0])}")
    print("Verified. The bootloader is now starting the new application.")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("image", help="raw binary linked for 0x2000 (or a 32 KB dump with --from-dump)")
    ap.add_argument("-p", "--port", required=True, help="serial port, e.g. /dev/tty.usbserial-0001")
    ap.add_argument("--trigger", action="store_true",
                    help="send 0xEA to the running original application first, "
                         "so it sets the update flag and resets into the bootloader")
    ap.add_argument("--pid", default="gcslbvj0",
                    help="8-character product ID stored at 0x7E01 (default: %(default)s)")
    ap.add_argument("--version", type=parse_version,
                    help="version to announce as X.Y.Z (default: installed version + 1)")
    ap.add_argument("--from-dump", action="store_true",
                    help="take the application part (0x2000..0x79FF) out of a full flash dump")
    ap.add_argument("--allow-settings-overwrite", action="store_true",
                    help="allow images that reach into the settings sector at 0x7A00")
    ap.add_argument("--wait", type=float, default=30.0,
                    help="seconds to wait for the bootloader (default: %(default)s)")
    ap.add_argument("-v", "--verbose", action="store_true", help="print raw frames")
    args = ap.parse_args()

    pid = args.pid.encode()
    if len(pid) != 8:
        ap.error("--pid must be exactly 8 characters")
    end = OTA_STATE_ADDR if args.allow_settings_overwrite else SETTINGS_ADDR
    try:
        image = load_image(args.image, args.from_dump, end - APP_BASE)
        with serial.Serial(args.port, 9600, bytesize=8, parity="N", stopbits=1,
                           timeout=0.1) as port:
            port.reset_input_buffer()
            link = Link(port, args.verbose)
            current = wait_for_bootloader(link, args.trigger, args.wait)
            upload(link, image, pid, args.version, current)
    except (OtaError, OSError, serial.SerialException) as exc:
        print(f"\nError: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
