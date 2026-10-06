# Thermal Label Printer — Working Protocol

Reverse-engineered from a `btsnoop` (Bluetooth HCI snoop) capture of the
official Android app, and then verified against the hardware. This document
contains **only** the commands this driver sends and that are confirmed working.
Anything untested or unused is omitted.

## Device

| Property | Value |
|---|---|
| Device label | "Thermal Label Printer", article `IAN 517574_2510`, Model `6326` |
| Self-reported model | `DP-L13` (what `10 FF 20 F0` answers) |
| Firmware | `V3.08` |
| Print head | 96 dots = 12 mm, 203 dpi → **12 bytes per raster row** |
| Paper | Gapped label/sticker tape (12 mm × 30 mm) |

## Connection

The printer exposes two radios. This driver uses **classic Bluetooth SPP** —
the same channel the official Android app uses.

| | Classic (used) | BLE (not used) |
|---|---|---|
| Name | `ML Printer` | `ML Printer_BLE` |
| Address | `55:55:09:22:90:81` | `5E:55:09:22:90:81` |
| Profile | Serial Port `0x1101`, RFCOMM channel **1** | GATT service `ff00` |

We open an RFCOMM socket to `{Addr: 55:55:09:22:90:81, Channel: 1}` and write
raw command bytes. The Bluetooth stack performs all RFCOMM framing (address /
control / length / credit / FCS) and splits the stream into frames, so the
application only writes the commands below.

**Why not BLE.** The printer also exposes a BLE GATT interface (service `ff00`,
write characteristic `ff02`) and it does answer the same `10 FF …` commands
there — but over BLE it misbehaves when printing: it drives the tape backwards,
drops the first characters of the label and can run the mechanism into its
stop. The official Android app does not use the BLE path either; it talks over
classic SPP, which is what this driver follows. For reference, both radios are
listed above.

## Command format

All control commands begin with `10 FF` (`0x10` = DLE, `0xFF` = prefix byte).

## Queries and settings

```
10 FF 40                  status bitfield (1 byte)
10 FF 20 F0               model            -> "DP-L13"
10 FF 20 F1               firmware         -> "V3.08"
10 FF 20 F2               serial           -> "L13261922621"
10 FF 50 F1               battery          -> 2 bytes, last = percent
10 FF 70                  everything       -> name|classicMAC|BLEMAC|firmware|serial|battery
10 FF 13                  auto power-off timer -> 1 byte, minutes
10 FF 12 <hi> <lo>        set that timer   -> answers "OK"  (16-bit big-endian minutes)
```

`10 FF 12` with a value of 0 is **not** "never": on the documented C&Co 3128 /
D11s it makes the printer switch itself off almost immediately, but **on this
printer it does not** - writing 0 only zeroed the timer, and the printer was
still answering 90 s later. (It is not "never" here either; it is simply not a
setting the Android app offers.)

The timer does work: after that many minutes without Bluetooth activity the
printer switches itself off. So the value is a **strategy, not a command** - a
small one suits "print a label and let it shut itself down", while a comfortable
one (5-15 minutes) suits a session you keep coming back to. (`10 FF 04` is a
factory reset, which this driver never sends.)

## Print sequence (exactly what the driver sends)

```
10 FF 10 00 01            set density  (0 = light, 1 = medium, 2 = dark)
00 00 ×12                 wake (12 null bytes)
10 FF F1 03               enable printer
1D 76 30 00 wL wH hL hH   GS v 0 raster header
<raster data>             widthBytes × height bytes
1D 0C                     form feed / position to next label  (aligns the tape)
1B 4A 28                  feed 40 dots
10 FF F1 45               stop print job
```

For continuous (non-label) tape the driver replaces the two label commands with
a single feed:

```
1B 4A 50                  feed 80 dots
```

## Bitmap format (GS v 0, uncompressed)

8-byte header:

```
1D 76 30 00 wL wH hL hH
```

| Bytes | Meaning |
|---|---|
| `1D 76 30` | GS v 0 command |
| `00` | mode — 0 = normal |
| `wL wH` | width in **bytes**, little-endian (`0C 00` = 12 bytes = 96 dots) |
| `hL hH` | height in pixels/rows, little-endian |

Pixel data: 1 bit per pixel, 8 pixels per byte, **MSB first** (bit 7 = leftmost
pixel), dark pixel = 1. Total data = `widthBytes × height`.

## Notes

- The image is rendered 96 px wide, rotated 90° so the text reads along the
  tape, auto-fitted to the label; the label length is capped with `-len` (mm).
- Only the commands above are used. BLE (see the *Connection* section) and other
  control commands are not used.
