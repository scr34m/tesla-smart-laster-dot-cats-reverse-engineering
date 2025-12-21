# TESLA Smart Laser Dot Cats Reverse Engineering

This [laser cat game](https://www.teslasmart.com/tesla-smart-laser-dot-cats) contains 2 motors for X/Y, MCU which is communicatib with the Tuya's BT3L chip.
This laser cat game contains 2 motors for X/Y, MCU which is communicatib with the Tuya’s BT3L chip.

Issues to be fixed:
- switch off after 5 min
- no Tuya DP values are changed when stopped

The MCU markings are removed, but it was eventually identified as `hc32l110c6pa`, key parameters are:
- Package: TSSOP20
- Flash: 32KB (not locked)
- SRAM: 4KB
- External crystal: 32 MHz

SWD identification:
- DPIDR: 0x0bc11477 [lead to this related issue](https://github.com/pyocd/pyOCD/issues/1635)
- Flash base address: 0x10000000
- SRAM base address: 0x20000000

### Tuya Data Points

- 0x66: raw type, move to X/Y 
- 0x68: value type, play modes (small, medium, large)
- 0x69: boolean type, on/off

Useful resources for understanding how the Tuya BT chip talks to the MCU via UART:
- https://images.tuyacn.com/smart/aircondition/Guide-to-Interworking-with-the-Tuya-MCU.pdf
- https://developer.tuya.com/en/docs/iot/tuya-cloud-universal-serial-port-access-protocol?id=K9hhi0xxtn9cb
- https://developer.tuya.com/en/docs/iot/ble-module-mcu-development-overview?id=K9eigm1j036kk
- https://developer.tuya.com/en/docs/iot/tuya-cloud-universal-serial-port-access-protocol?id=K9eigf2el456o
- https://developer.tuya.com/en/docs/mcu-standard-protocol/Bluetooth-LE-Intergation-Base-Function?id=Kd3q32tjfcufw
- https://developer.tuya.com/en/docs/mcu-standard-protocol/Bluetooth-LE-MCU-OTA-Service?id=Kd3wc0s33242s

### Identified pins

```
          ______ 
start 1 -| o    |- 20 button
left  2 -|      |- 19 led
right 3 -|      |- 18 SWC
RST   4 -|      |- 17 SWD
XTAL1 5 -|      |- 16
XTAL2 6 -|      |- 15
VSS   7 -|      |- 14
      8 -|      |- 13 down
 VDD  9 -|      |- 12 UART TX
up   10 -|______|- 11 UART RX
```

### Code sections identified as

Bootloader part
```
00000000 a0 07 00 20     addr       DAT_200007a0                                     = ??
00000004 dd 00 00 00     addr       FUN_000000dc+1
00001174 main
```

Main code part
```
00002038 f7 20 00 00     addr       DAT_000020f7                                     = E7h
0000203c 69 42 00 00     addr       FUN_00004268+1
000020c8 -> BX -> 00006399h
00006398 main 
```

### OpenOCD DAP connection output

```
Info : STLINK V2J29S7 (API v2) VID:PID 0483:3748
Info : Target voltage: 3.138699
Info : clock speed 100 kHz
Info : SWD DPIDR 0x0bc11477
Info : [chip.cpu] Cortex-M0+ r0p1 processor detected
Info : [chip.cpu] target has 4 breakpoints, 2 watchpoints
Info : [chip.cpu] Examination succeed
Info : [chip.cpu] starting gdb server on 3333
[chip.cpu] halted due to debug-request, current mode: Thread
xPSR: 0x61000000 pc: 0x000053e0 msp: 0x20000610
Info : Listening on port 3333 for gdb connections
Info : Listening on port 6666 for tcl connections
Info : Listening on port 4444 for telnet connections
AP # 0x0
                AP ID register 0x04770031
                Type is MEM-AP AHB3
MEM-AP BASE 0xe00ff003
                Valid ROM table present
                Component base address 0xe00ff000
                Peripheral ID 0x04000bb4c0
                Designer is 0x23b, ARM Ltd
                Part is 0x4c0, Cortex-M0+ ROM (ROM Table)
                Component class is 0x1, ROM table
                MEMTYPE system memory present on bus
        ROMTABLE[0x0] = 0xfff0f003
                Component base address 0xe000e000
                Peripheral ID 0x04000bb008
                Designer is 0x23b, ARM Ltd
                Part is 0x008, Cortex-M0 SCS (System Control Space)
                Component class is 0xe, Generic IP component
        ROMTABLE[0x4] = 0xfff02003
                Component base address 0xe0001000
                Peripheral ID 0x04000bb00a
                Designer is 0x23b, ARM Ltd
                Part is 0x00a, Cortex-M0 DWT (Data Watchpoint and Trace)
                Component class is 0xe, Generic IP component
        ROMTABLE[0x8] = 0xfff03003
                Component base address 0xe0002000
                Peripheral ID 0x04000bb00b
                Designer is 0x23b, ARM Ltd
                Part is 0x00b, Cortex-M0 BPU (Breakpoint Unit)
                Component class is 0xe, Generic IP component
        ROMTABLE[0xc] = 0x00000000
                End of ROM table
Info : Listening on port 6666 for tcl connections
Info : Listening on port 4444 for telnet connections
```

Dump flash and sram over telnet port:
```
dump_image flash.bin 0x00000000 0x8000
dump_image flash_2.bin 0x20000000 0x1000
```

### Debugging over SWD

```
C:\ST\STM32CubeIDE_1.19.0\STM32CubeIDE\plugins\com.st.stm32cube.ide.mcu.externaltools.gnu-tools-for-stm32.13.3.rel1.win32_1.0.0.202411081344\tools\bin>arm-none-eabi-gdb.exe

# init
set breakpoint always-inserted on
set disassemble-next-line on
target remote localhost:3333
monitor reset halt

info breakpoint
hbreak *0x00003554
hbreak *0x000035b6
hbreak *0x0000700E
mdb 0x2000002e
```

### Patch

The timeout can be increased by changing the value `0x2D` to `0xFF`, which extends the timeout from 300 seconds to 510 seconds.
```
00003548 ff 21           movs       r1,#0xff
0000354a 2d 31           adds       r1,#0x2d
```

After 5 minutes, the motors and the laser stop. The corresponding firmware code is located at address  `0x000034b0`.
In the 300 second check branch, the counter is not reset because the function at address `0x0000345c` is not called.

Chosen address to insert the custom jump code:
```
0000356c 01 f0 86 fa     bl         FUN_00004a7c                                     undefined FUN_00004a7c()
```

which transfers control to our code at address `0x00007000`
```
0000356c 03 F0 48 FD     bl         #0x3A94
```

Here we call the reset counter, and DP update for `0x68`
```
00007000 FD F7 3C FD     bl         #-0x2584 @0x00004a7c
00007004 00 21           movs       r1,#0x0
00007006 03 20           movs       r0,#0x3
00007008 FC F7 28 FA     bl         #-0x3BAC @0x0000345c
0000700C 00 21           movs       r1,#0
0000700E 68 20           movs       r0,#0x68
00007010 FF F7 D0 FB     bl         #-0x85C  @0x000067b4
00007014 70 47           bx         lr
```

### Flashing the code with pyocd

To flash the chip [IOsetting's hc32l110-template](https://github.com/IOsetting/hc32l110-template) was the source it how is possible.

`.\pyocd-windows-0.42.0\pyocd.exe load .\flash_600.bin -t hc32l110c6pa --config .\pyocd.yaml`

The `pyocd.yaml` configuration file includes only information about the pack.
```
pack:
  - HDSC.HC32L110.1.0.3.pack
```