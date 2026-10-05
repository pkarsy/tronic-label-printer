# tronic-label-printer

> **No warranty, no liability.** This is a personal tool, shared in case it is
> useful. Use it at your own risk. See [LICENSE](LICENSE). Not affiliated with
> Lidl or the printer's manufacturer; *Tronic* and *Lidl* are their owners'
> trademarks, named here only to say what this works with.

![A label printed by this tool: the word LABEL in bold black capitals, filling
the 12 mm × 30 mm label](label.png)

Print small text labels on the **Tronic Thermal Label Printer** (Lidl's own
brand; article IAN 517574_2510, labelled Model 6326) over **classic Bluetooth
SPP**, straight from the command line — no phone app.

```sh
./label "Hello World"
```

## Why this instead of the app?

- **Much faster.** Type a line and press Enter — no app to open, no menus, no
  re-pairing dance. (Session mode keeps the connection open for a whole session.)
- **No manual font fiddling.** The label is tiny (12 mm × 30 mm), so there is
  very little room. The app makes you pick a font and a size by hand to use it;
  here the text is auto-fitted to fill the label.

It earned its keep on day one: about 200 labels in, with the printer bought to
label boxes of electronic parts, I have had no reason to open the phone app
again.

## Requirements

- Linux with BlueZ. The tool finds the printer by its Bluetooth name
  (**"ML Printer"**), so no MAC address is needed — and **no pairing is
  required**. It remembers the address it found (in `$XDG_CACHE_HOME/label/addr`)
  and uses that from then on, which is worth having because BlueZ forgets a
  printer it merely discovered after a couple of minutes. A remembered address is
  dropped only if it turns out to be invalid: a printer that is merely switched
  off (or busy) keeps it, so an idle printer never costs a scan. `-addr` pins a
  specific address instead, and is never cached. Note that `ML Printer_BLE` is
  the wrong device: this tool uses Bluetooth Classic, not BLE.
- Go 1.26+ to build.

## Platform

**Linux only** — this is a personal tool, not a product, so other systems are
not planned. Everything that touches the system is Linux-specific:

- the connection uses the kernel's RFCOMM socket (`BTPROTO_RFCOMM`,
  `SockaddrRFCOMM`) — macOS has no public equivalent, and Windows exposes
  Bluetooth serial as a COM port instead;
- the printer is found through BlueZ (`bluetoothctl`);
- the idle-exit restores the terminal with Linux termios (`TCGETS`/`TCSETS`);
- the font is read from `/usr/share/fonts/truetype/dejavu/`.

It does not build on Windows (there is no `x/sys/unix` there), and **WSL2 is not
a way around that**: the stock WSL2 kernel is built without `CONFIG_BT`, so its
Linux has no Bluetooth adapter at all and the printer is unreachable.

## Build

Pure Go — **no cgo and no C toolchain** required. A bare `go build` gives you
`label`:

```sh
go build
```

Or, for a smaller binary:

```sh
./build.sh          # CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath
```

## Usage

```sh
./label "MATHEMATICS"        # print it, then stay for more labels
./label -once "MATHEMATICS"  # print just that one and exit
./label                      # session mode: connect once, one label per line
./label -status              # report the printer's state (prints no label)
./label -h                   # options + text rules
```

You rarely switch the printer on for a single label — a storage box wants one on
each side — so when you give TEXT **at a terminal** it prints that label and then
leaves you connected, ready for the next one. Under a pipe or a script it prints
one and exits, so pipelines are unaffected; `-once` asks for that explicitly.

### Preview without printing

`-save-image` writes the label out as a `.png` in the current directory instead
of printing it, named after the text. It never touches Bluetooth, so it can check
a layout with no printer and no tape:

```sh
$ ./label -save-image "Ag 3%"
Wrote Ag 3%.png (240x96 dots, the exact bitmap that would be printed).

$ ./label -save-image -border "Ag 3%"
Wrote Ag 3%.png (242x98 dots, the exact bitmap that would be printed).
```

A label is mostly white, so on a white page its edges can simply vanish in a
viewer. `-border` draws a one-pixel frame so the extent stays visible.

A label is only 96 dots across, which is small in a README. `-scale N` enlarges
the picture by a whole number — by nearest neighbour, so every dot stays a crisp
square and nothing gets blurred. Both together are the recipe for documentation
images:

```sh
$ ./label -save-image -border -scale 3 "Ag 3%"
Wrote Ag 3%.png (726x294 pixels, 3x the printed bitmap).
```

The `.png` is turned back so the text reads horizontally (the reverse of the
printer's own rotation), which makes it easy to look at — what would be printed is
not affected, only the picture. The picture is the same bitmap the raster comes
from, so what you see is what would come out. If the file already exists it asks
before overwriting, and anything that is not a clear `y` (including no answer at
all) means no.

### Session mode

Connecting takes a few seconds, so for many labels run it with no text argument:
it connects **once**, then prints a label for every line you type. It exits on
**Ctrl-D** or after `-idle` seconds with no input (default 10 minutes — long
enough to find the next box). Press the **Up arrow** to recall a previous label
(printing the same label again is just Up + Enter). It reads the battery up front,
and then again every half of the printer's auto-off — which doubles as a
**keep-alive**. That matters because the printer's auto-off timer counts Bluetooth
silence and ignores an open link: without it the printer would switch itself off
underneath you mid-session. The same reading gives an early **low-battery
warning**, before it dies on you.

If it goes away anyway — battery flat, or you pressed its button — the session
notices and exits with a line saying so, rather than sitting at a prompt that can
no longer print. Start it again; the address is cached, so it reconnects
immediately. And when the session does end, the printer is free to power itself
off again on its own timer.

**For a long session** — a drawer full of components, say — put the printer on its
charger, switch it on, and give the session room to breathe:

```sh
./label -idle 3600
```

It stays awake because the session keeps asking it things, and keeping it awake
costs about 2% of battery every four minutes (~30% an hour), so the charger is what
makes a long run practical. `-idle 3600` means "do not give up until an hour has
passed with nothing typed", so pausing to find the next box will not end it. If the
LED starts blinking between green and red, that is normal on the charger — see
*Troubleshooting*.

```
$ ./label
Using paired device with MAC 55:55:09:22:90:81
Battery: 72%
Ready. Type text + Enter to print a label.
label> Book One
✓ Book One
label> Book Two
✓ Book Two
```

### Printer status

`-status` connects, asks the printer how it is, and prints the answer — handy
before a long session, or when a label comes out blank:

```
$ ./label -status
Using paired device with MAC 55:55:09:22:90:81
  Model:     DP-L13
  Firmware:  V3.08
  Device ID: L13261922621
  Battery:   82%
  Auto-off:  5 min
  Paper:     OK
```

`Battery:` is the bare reading, with nothing inferred from it: the firmware has
no charging flag, and the same printer on the charger has been caught reporting
both its true level and a flat 100%, so a comment about charging would be a guess.
Nor does the charger change anything else — the auto-off timer runs while it is
plugged in too, so being on charge is no reason to expect it awake.

The printer's own settings are here too: the auto-off timer shown above is read
by `-status` and changed with `-auto-off N`.

### Text rules

| Input | Result |
|---|---|
| a space | allowed split point for two lines |
| a long single word | auto-split in two lines too |
| `-` | a plain character — type it yourself, e.g. `MATH- EMATICS` |

A two-line layout is used **only when it makes the font clearly bigger** (at
least 1.5×), so short titles stay on one line and only long text wraps.
`-lines 1` disables the split entirely; `-font N` forces a fixed font size.

## Why text only?

The labels are tiny: about **12 mm × 30 mm**, i.e. a 96 × 240 dot bitmap at
203 dpi. On an area that small a photo, logo or QR-ish graphic collapses into
unreadable mud, so images would waste label after label for no gain. A short
line of text is what actually fits and reads — so this tool renders text only,
and sizes/positions it to fill the label.

## Troubleshooting

- **"device or resource busy"** — the printer is usually still releasing a
  previous link (or the phone app is connected). Connection retries
  automatically for a while; if it persists, wait a few seconds and re-run.
- **"printer reports NO PAPER"** — the roll is empty (or the tape is wound all
  the way in). Load a new label roll; the tool checks the printer status before
  every label, so it never silently prints nothing.
- **"no Bluetooth device named ... found"** — the printer is off or out of
  range (or has never been discovered). Switch it on and retry, or point
  `-name` / `-addr` at it.
- **"the printer is on ... but its Bluetooth Classic radio did not"** — the
  printer is powered on, but something else holds its Classic link — normally
  the phone app. Close it and retry. (Its BLE radio stays visible while the
  Classic one is taken, which is how the tool knows the printer is on.)
- **Nothing prints** — use the *Bluetooth Classic* device (`ML Printer`), not
  the `*_BLE` one.
- **LED alternating green and red** — connecting while the printer sits on its
  charger with a full battery has been seen to start this. It looks like a fault
  and is not one: it stops when the session ends or the charger comes out. The
  firmware does not document the behaviour, so nothing here depends on it. (My own
  remedy: I stand the printer with its button and LED facing the desk, where the
  blinking cannot bother me. The LEDs on this unit are unusually bright, so a hint
  that was probably meant to be discreet ends up insistent.)

## Paper

- Built and tested only with the **official Tronic (Lidl) labels** (the 12 mm
  tape that comes with the printer). Other label stock may have a different
  gap/size, so the fit and the auto-alignment may differ.

## Options, usually not needed

```
-w N        print head width in pixels (default 96)
-font N     font size (default 0 = auto-fill)
-len MM     label length in mm (default 30)
-label      gapped tape: advance to the next label (default true)
-addr MAC   use a fixed Bluetooth Classic address (skips the name lookup)
-name NAME  Bluetooth name to look up (default "ML Printer")
-idle SEC   session mode idle timeout (default 600 = 10 minutes)
-once       with TEXT: print that label and exit, do not stay for more
-lines N    maximum text lines (default 2)
-status     show the printer status (model, battery, paper) and exit
-auto-off N set the printer's auto power-off timer to N minutes (N >= 1; leave
            the flag out to change nothing). A small value = print and let it
            shut itself down; 5-15 min suits a session you keep coming back to.
-save-image with TEXT: write the label as a .png here instead of printing
-border     with -save-image: one-pixel black frame around the image
-scale N    with -save-image: enlarge by N (1-16), nearest neighbour
```

## Links

- **Manual** —
  [TRONIC Thermal Label Printer, IAN 517574_2510](https://media.sit-connect.com/public/articlemanual/093e1646-7644-47c4-aa62-5c872515df9d.pdf)
  — Lidl's own PDF (verified working October 2026). It is a single 508-page file
  holding **every EU language** (hence the 40 MB); the cover carries the IAN
  above, and the Greek section is near the front.
- **Lidl service page** —
  https://exypiretisi-pelaton.lidl-hellas.gr/SelfServiceGR/s/product2?productId=01tJ9000002MgWrIAK&q=IAN%20517574_2510
  — if the direct link ever dies.

  There is no stable Lidl *product* page for this printer, and their service site
  is a JavaScript app: the PDF sits behind it with no address of its own to copy,
  which is why the link above came from the download itself. It is a third-party
  link, so it may break as time passes — the IAN printed on the device is the
  durable identifier, and any Lidl country's service site finds the manual with
  it.
- **PROTOCOL.md** — the command set, reverse-engineered from the official app's
  Bluetooth traffic and verified against the hardware.

## Development

Created with the **Reasonix** coding agent using **DeepSeek** AI, and minimal manual editing.

