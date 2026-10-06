package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/sys/unix"
)

// The printer exposes TWO radios:
//
//   - Bluetooth Classic (SPP/RFCOMM, name "ML Printer", 55:55:09:22:90:81).
//   - BLE (name "ML Printer_BLE", 5E:55:09:22:90:81): GATT ff00, write ff02.
//
// We use CLASSIC SPP/RFCOMM (channel 1), exactly like the official Android app.
// This was confirmed with a btsnoop capture of that app: SDP answers with the
// Serial Port profile (0x1101) and a service on RFCOMM channel 1 ("SPP slave"),
// and the conversation runs over L2CAP PSM 3 (RFCOMM).
//
// The very same "10 FF ..." commands and GS v 0 raster travel over SPP. The
// RFCOMM framing (address/control/length/credit/FCS) is done by the kernel, so
// we simply write the command stream to the socket.
// The printer's Bluetooth NAME. The MAC address differs from unit to unit, so
// hardcoding one would make this tool work for exactly one person - we look the
// printer up by name instead (and -addr still overrides it).
const printerName = "ML Printer"

// The RFCOMM channel the printer advertises for SPP (from its SDP record).
const sppChannel = 1

// Largest enlargement -save-image will produce. A label is 96 dots across, so
// beyond a small factor the file is just huge.
const maxScale = 16

// A session warns once when the battery falls to this percentage, before the
// printer has a chance to die mid-label.
const batteryWarnBelow = 15

// Print settings.
const (
	// Density: 0 = light, 1 = medium, 2 = dark.
	density = 0x01
	// Paper feed at the end of a job (non-label mode), in dots.
	feedDots = 80
	// After 1D 0C the official Android app feeds another 40 dots (1B 4A 28).
	labelFeedDots = 0x28

	// Print head width in pixels: 96 dots = 12 mm = 12 bytes per raster row.
	// The printer reports its own model on request (10 FF 20 F0).
	//
	// NOTE: a 384-dot head (48 bytes/row) is a DIFFERENT model - with the wrong
	// width the printer accepts the raster but prints nothing (the text falls
	// outside the printable area). This is also settable with -w.
	printWidthPx = 96

	// The raster is rotated 90° clockwise, so the text reads along the tape:
	// the rendered text is a page laid out across the label - its height on the
	// print head (96 dots = 12 mm), its width along the label - and rotating it
	// turns that into one raster row per step of the tape.
	//
	// This is not a preference: any other angle puts the raster on the head
	// sideways. At 0° and 180° the raster is as wide as the rendered text - 213
	// to 224 dots, i.e. 27-28 bytes per row, for ordinary titles at the default
	// -len 30 - and anything past a single character overflows the head, which
	// is 96 dots = 12 bytes wide.
	rasterRotateDeg = 90

	// Minimum characters per line for the AUTOMATIC word split. A short word
	// (e.g. 5 letters) is not split - it would become two tiny fragments that
	// are hard to read.
	minSplitRunes = 3

	// A two-line layout is used only when its font is at least this much bigger
	// than the single-line font; otherwise the text stays on one line. This
	// keeps short titles (e.g. two words) on a single line.
	twoLineMinGain = 1.5
)

// Job enable/stop commands, SPECIFIC TO A MODEL FAMILY. This printer belongs to
// the Base/Lujiang family (LuckPrinter SDK) and uses the pair
// F1 03 / F1 45 (see PROTOCOL.md - "Enable printer (Lujiang mode = 3)").
// AiYin (D11s, D12 and relatives) wants FE 01 / FE 45. With the wrong pair the
// printer silently accepts all the data, turns the motor and prints NOTHING.
var (
	cmdEnable = []byte{0x10, 0xff, 0xf1, 0x03}
	cmdStop   = []byte{0x10, 0xff, 0xf1, 0x45}
)

// 1D 0C - form feed / advance to the next label: it moves the paper up to the
// next gap (the start of the next label) and thus aligns the following print.
var labelNext = []byte{0x1d, 0x0c}

// Status bits answered by the "10 FF 40" query.
//
// Only the paper bit carries real information on this printer: the cover and head
// bits stay put whether the cover is open or closed and whether the head is hot
// or cold, so we deliberately do not report them - a guard built on them would be
// fiction. The battery reading is likewise not to be trusted about charging:
// there is no charging flag, and the same printer on the charger has been caught
// reporting both 100% and its true level. So the battery is reported as the bare
// percentage, with nothing inferred from it. Nor does the charger keep the
// printer awake: its auto-off timer runs while it is plugged in too, so never
// infer anything about the printer's state from the fact that it is charging.
const (
	statusNoPaper = 0x04
	statusLowBatt = 0x08 // never observed set - unverified
)

// Timeouts.
const (
	connectTimeout = 12 * time.Second
	// The printer sometimes holds a previous link for 15-30s and connect fails
	// with "device or resource busy" - so we give it several chances.
	connectAttempts = 6
	connectRetryGap = 5 * time.Second
)

// config gathers the settings so they can be passed easily into session mode
// (many labels over a single connection).
type config struct {
	width      int
	fontSize   float64
	labelLenMM float64
	fontFile   string
	label      bool
	maxLines   int
}

func main() {
	width := flag.Int("w", printWidthPx, "print head width in pixels (96 for this printer)")
	fontSize := flag.Float64("fontsize", 0, "font size (0 = auto-fill the label)")
	labelLenMM := flag.Float64("len", 30, "label length in mm (0 = fill width only)")
	fontFile := flag.String("fontfile", "", "font file to use instead of the one found on the system")
	label := flag.Bool("label", true, "gapped tape: advance to the next label")
	name := flag.String("btname", printerName, "Bluetooth Classic name to look up when -addr is empty; the printer's own name is fixed, so another name probably means a different revision")
	addr := flag.String("addr", "", "Bluetooth Classic address (default: look up -btname)")
	idle := flag.Int("idle", 600, "session mode: exit after N seconds without input")
	maxLines := flag.Int("lines", 2, "maximum text lines (1 = never split in two)")
	status := flag.Bool("status", false, "show printer status (model, battery, paper) and exit")
	autoOff := flag.Int("auto-off", 0, "set the printer's auto power-off timer to N minutes (N >= 1); the printer keeps it until changed")
	saveImage := flag.Bool("save-image", false, "one-shot mode: write the label as a .png here instead of printing")
	border := flag.Bool("border", false, "with -save-image: put a one-pixel black frame around the image")
	scale := flag.Int("scale", 1, "with -save-image: enlarge by this whole-number factor (1-16)")
	once := flag.Bool("once", false, "with TEXT: print that label and exit, do not stay for more")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [TEXT]\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(os.Stderr, "  Without TEXT: session mode - connects once and prints one label per stdin line.")
		fmt.Fprintln(os.Stderr, "  With TEXT at a terminal: print it, then stay for more (-once, or a pipe, prints one and exits).")
		fmt.Fprintln(os.Stderr, "  Text rules: a space (or a long word) allows a two-line split, used when it makes the font bigger; -lines 1 keeps one line.")
		fmt.Fprintln(os.Stderr, "  -status: just report the printer's state (does not print a label).")
		fmt.Fprintln(os.Stderr, "  -auto-off N: set the printer's auto power-off timer to N minutes (N >= 1) and exit.")
		fmt.Fprintln(os.Stderr, "     A session holds the printer awake while it runs, so once you stop, a small value is fine.")
		fmt.Fprintln(os.Stderr, "     The printer keeps it until changed: it is a setting stored in the printer, not a per-run flag.")
		fmt.Fprintln(os.Stderr, "  -save-image: with TEXT, write the label as a .png in the current directory instead of printing.")
		fmt.Fprintln(os.Stderr, "  -border: with -save-image, draw a one-pixel black frame so the label's edges are visible.")
		fmt.Fprintln(os.Stderr, "  -scale N: with -save-image, enlarge the picture by N (1-16) - nearest neighbour, so dots stay crisp.")
		fmt.Fprintln(os.Stderr, "  The printer is looked up by Bluetooth name (-btname); it advertises \"ML Printer\",")
		fmt.Fprintln(os.Stderr, "     a fixed name, so that default rarely needs changing. -addr pins a fixed address instead.")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg := config{*width, *fontSize, *labelLenMM, *fontFile, *label, *maxLines}

	// -auto-off is a write, and 0 is NOT a way to ask for "never": the printer
	// rejects it as a setting. Distinguish "not given" from "given 0" so a
	// mistake is reported instead of silently doing nothing.
	given := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "auto-off" {
			given = true
		}
	})
	if given && *autoOff < 1 {
		fmt.Println("Error: -auto-off needs a value of at least 1 minute.")
		fmt.Println("       Anything below 1 is not a valid setting - it is not \"never\", and it")
		fmt.Println("       does not switch the printer off either.")
		fmt.Println("       Use 5-15 when you keep coming back to the printer, or 1-2 for a")
		fmt.Println("       print-and-shut-down setup.")
		os.Exit(2)
	}

	// A bad -fontfile is the user's own mistake, and it can be reported before
	// anything talks to the printer - otherwise the tool would scan, connect and
	// only then refuse to draw.
	if *fontFile != "" {
		if err := loadFontFile(*fontFile); err != nil {
			fmt.Printf("Error: cannot use -fontfile %s: %v\n", *fontFile, err)
			fmt.Println("       A TrueType .ttf works; a .ttc collection does not (the font parser")
			fmt.Println("       cannot read one). With no -fontfile, the font is found on the system.")
			os.Exit(2)
		}
	}

	if *saveImage {
		if flag.NArg() == 0 {
			fmt.Println("Error: -save-image needs TEXT - it is the one-shot mode, not session mode.")
			os.Exit(2)
		}
		// A label is 96 dots across, so anything past a small factor is just a
		// huge file; refuse it rather than allocate it.
		if *scale < 1 || *scale > maxScale {
			fmt.Printf("Error: -scale must be between 1 and %d.\n", maxScale)
			os.Exit(2)
		}
		// No printer needed at all: nothing below this point is reached.
		runSaveImage(cfg, strings.Join(flag.Args(), " "), *border, *scale)
		return
	}

	target, err := resolveAddr(*addr, *name)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	if given {
		runSetAutoOff(target, *name, *autoOff)
		return
	}
	if *status {
		runStatus(target, *name)
		return
	}

	// No text argument -> session mode: one connection, one label per line.
	// With TEXT it depends on who is asking: a human at a terminal has almost
	// certainly more than one label to print (a box wants front and back), so we
	// print theirs and then leave them at the prompt - the connection and the
	// keep-alive are already up. A pipe or a script keeps the old behaviour and
	// exits, and -once asks for that explicitly.
	if flag.NArg() == 0 {
		runSession(target, *name, cfg, time.Duration(*idle)*time.Second, "")
		return
	}
	text := strings.Join(flag.Args(), " ")
	if *once || !isTerminal(int(os.Stdin.Fd())) {
		runOnce(target, *name, cfg, text)
		return
	}
	runSession(target, *name, cfg, time.Duration(*idle)*time.Second, text)
}

// isTerminal asks the kernel, rather than looking at the file mode: /dev/null is
// a character device too, and under a pipe or a script we must not sit at a
// prompt waiting for typist who is not there.
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// runOnce prints a single label and exits.
func runOnce(addr, name string, cfg config, text string) {
	fmt.Printf("Building label for: '%s'...\n", text)
	conn, err := connect(addr, name)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()
	if err := printText(conn, cfg, text); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Printed successfully.")
}

// runSetAutoOff sets the printer's auto-off timer and reports what the printer
// now holds - read back, so the answer is the printer's, not ours.
func runSetAutoOff(addr, name string, minutes int) {
	conn, err := connect(addr, name)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()

	if err := setAutoOff(conn, minutes); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	if v := autoOffMinutes(conn); v >= 0 {
		fmt.Printf("Auto-off set to %d min.\n", v)
		return
	}
	fmt.Println("Auto-off sent, but the printer did not report the value back.")
}

// runStatus connects and reports the printer's state (model, serial, battery,
// paper). It prints no label.
func runStatus(addr, name string) {
	conn, err := connect(addr, name)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()

	if v, ok := conn.queryText([]byte{0x10, 0xff, 0x20, 0xf0}); ok {
		fmt.Printf("  Model:     %s\n", v)
	}
	if v, ok := conn.queryText([]byte{0x10, 0xff, 0x20, 0xf1}); ok {
		fmt.Printf("  Firmware:  %s\n", v)
	}
	// The "10 FF 70" record is "name|classicMAC|BLEMAC|firmware|serial|battery";
	// the Android app shows that serial as the "Device ID".
	if v, ok := conn.queryText([]byte{0x10, 0xff, 0x70}); ok {
		if f := strings.Split(v, "|"); len(f) >= 5 {
			fmt.Printf("  Device ID: %s\n", f[4])
		}
	}
	// Battery level, live (the record above may hold a stale snapshot).
	if v := batteryLevel(conn); v != "" {
		fmt.Printf("  Battery:   %s\n", v)
	}
	// Auto power-off timer, in minutes.
	if v := autoOffMinutes(conn); v >= 0 {
		fmt.Printf("  Auto-off:  %d min\n", v)
	}

	st, ok := conn.readStatus()
	if !ok {
		fmt.Println("  Paper:     unknown (the printer did not answer)")
		return
	}
	// Only the paper bit carries real information on this printer: cover and
	// head never change, so reporting them would just be noise.
	paper := "OK"
	if st&statusNoPaper != 0 {
		paper = "NO PAPER"
	}
	fmt.Printf("  Paper:     %s\n", paper)
	if st&statusLowBatt != 0 {
		fmt.Println("  Warning:  battery is low")
	}
}

// runSession connects ONCE and prints one label for every line read from stdin.
// An empty line prints nothing; it exits after `idle` with no input, or on Ctrl-D.
// A non-empty first is printed before the prompt appears: that is `label TEXT`
// typed at a terminal, where a second label is almost always wanted too.
func runSession(addr, name string, cfg config, idle time.Duration, first string) {
	conn, err := connect(addr, name)
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close()

	if first != "" {
		// Print theirs, then hand over the prompt. No full banner: the connection
		// is already up and the keep-alive is running, so announcing a fresh
		// start would mislead about what just happened.
		if err := printText(conn, cfg, first); err != nil {
			fmt.Printf("Print error: %v\n", err)
		} else {
			fmt.Printf("✓ %s\n", first)
		}
		fmt.Println("Connected. Type another label, or Ctrl-D to stop.")
	} else {
		// One battery reading for the whole session, before the prompt: the print
		// loop itself stays quiet.
		if v := batteryLevel(conn); v != "" {
			fmt.Printf("Battery: %s\n", v)
		}
		fmt.Println("Ready. Type text + Enter to print a label.")
		fmt.Println("Up arrow recalls previous labels.")
		fmt.Printf("Exit with Ctrl-D or after %s idle.\n", idle)
	}

	lr := newLineReader()
	defer lr.Close()

	// The reader does all the work (read -> print) so that we never write on top
	// of the prompt. The main goroutine just measures inactivity.
	type event struct{ eof bool }
	activity := make(chan event, 1)
	// Set while a job is in flight, so the keep-alive below can hold off: its
	// reply would otherwise land in the middle of the "OK" counting waitDone does.
	var printing atomic.Bool
	go func() {
		for {
			s, err := lr.ReadLine()
			if err != nil {
				if errors.Is(err, readline.ErrInterrupt) {
					activity <- event{}
					continue
				}
				activity <- event{eof: true}
				return
			}
			activity <- event{}
			text := strings.TrimSpace(s)
			if text == "" {
				continue
			}
			printing.Store(true)
			err = printText(conn, cfg, text)
			printing.Store(false)
			if err != nil {
				fmt.Printf("Print error: %v\n", err)
				continue
			}
			fmt.Printf("✓ %s\n", text)
		}
	}()

	// Keep the printer awake while the session is open. Its auto-off timer counts	// Bluetooth silence and does NOT care that our link is up (measured: it
	// powers down mid-session), but an ordinary query does reset it (measured
	// too - 4+ minutes alive on a 1-minute timer). So ask for the battery every
	// so often, which doubles as an early warning before it runs out. Half of
	// what the printer is set to, and never 0.
	every := 60 * time.Second
	if m := autoOffMinutes(conn); m > 0 {
		every = time.Duration(m) * time.Second / 2
		if every < 30*time.Second {
			every = 30 * time.Second
		}
		if every > 5*time.Minute {
			every = 5 * time.Minute
		}
	}
	ping := time.NewTicker(every)
	defer ping.Stop()
	var warnedLow bool

	timer := time.NewTimer(idle)
	defer timer.Stop()
	// While we sit at the prompt the printer can switch ITSELF off: its auto-off
	// timer counts Bluetooth silence, and it does not care that our link is
	// open (verified: it powers down mid-session). Checking the socket tells us
	// the moment it goes - far better than staying at a prompt that can only
	// fail. The peek sends nothing, so it cannot itself count as activity.
	link := time.NewTicker(time.Second)
	defer link.Stop()
	for {
		select {
		case ev := <-activity:
			if ev.eof {
				fmt.Println("Exiting.")
				return
			}
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(idle)
		case <-ping.C:
			// The keep-alive. Holding off while a job is in flight keeps its
			// reply out of the "OK" counting; a silent answer here means the
			// printer is gone, and the link ticker above reports that.
			if printing.Load() {
				continue
			}
			if pct := batteryPercent(conn); pct >= 0 && pct <= batteryWarnBelow && !warnedLow {
				warnedLow = true
				fmt.Printf("\nBattery at %d%% - the printer may switch itself off before long.\n", pct)
			}
		case <-link.C:
			if !conn.linkAlive() {
				fmt.Println("\nThe printer has switched itself off (its auto-off timer).")
				lr.RestoreTerminal()
				os.Exit(0)
			}
		case <-timer.C:
			fmt.Printf("\nIdle %s - exiting.\n", idle)
			// Exit with os.Exit because the deferred lr.Close() blocks while a
			// Readline is pending (it waits for ENTER) - and that leftover
			// process keeps the RFCOMM port, making the printer look "busy".
			// Restore the terminal by hand, since defers do not run.
			lr.RestoreTerminal()
			os.Exit(0)
		}
	}
}

// resolveAddr picks the Bluetooth address to talk to, in order of authority:
//
//  1. an explicit -addr (never cached - that is the user's choice, not ours),
//  2. BlueZ's paired list, then everything else BlueZ has seen,
//  3. what we remembered ourselves from an earlier run,
//  4. a discovery burst.
//
// Whatever it finds is remembered, so a printer BlueZ forgets (an unpaired one,
// after a couple of minutes) does not cost a scan on every run - and so losing a
// pairing costs nothing either. The address differs on every unit, which is why
// the name is the thing worth writing down.
func resolveAddr(addr, name string) (string, error) {
	if addr != "" {
		return addr, nil
	}
	// 1 + 2: BlueZ's own lists (paired first, then everything it has seen).
	// Remember the answer either way, so losing a pairing costs nothing later.
	if mac, paired, found, _ := lookupCached(name); found {
		rememberAddr(name, mac)
		fmt.Println(deviceLine(mac, paired))
		return mac, nil
	}
	// 3: what we remembered from an earlier run.
	if mac := cachedAddr(name); mac != "" {
		fmt.Println("Using cached device with MAC " + mac)
		return mac, nil
	}
	// 4: a discovery burst.
	found, err := scanFor(name)
	if err != nil {
		return "", err
	}
	rememberAddr(name, found)
	fmt.Println(deviceLine(found, false))
	return found, nil
}

// deviceLine says how the address was found: BlueZ's paired list, or merely a
// discovery. The distinction matters, because an unpaired printer prints just
// as well - the output must not claim a pairing that is not there.
func deviceLine(mac string, paired bool) string {
	if paired {
		return "Using paired device with MAC " + mac
	}
	return "Using discovered device with MAC " + mac
}

// scanFor looks for the name with a discovery burst, used only when neither
// BlueZ nor our own cache knows the printer. The name is the part that is the
// same on every unit; "ML Printer_BLE" (the BLE radio we do not use) has a
// different name, so an exact match selects the Classic one that SPP needs.
func scanFor(name string) (string, error) {
	if _, _, _, btctlOK := lookupCached(name); !btctlOK {
		return "", errors.New("cannot talk to BlueZ (bluetoothctl) - is it installed and running?")
	}
	fmt.Printf("Scanning for %q (up to 8s)...\n", name)
	scan()
	if mac, _, found, _ := lookupCached(name); found {
		return mac, nil
	}
	// Its BLE twin answering means the printer IS powered on and in range, so
	// "off?" is ruled out: what is left is a Classic radio that will not
	// answer - which is exactly what happens when the phone app holds it.
	if _, _, ble, _ := lookupCached(name + "_BLE"); ble {
		return "", fmt.Errorf("the printer is on (%q answered) but its Bluetooth Classic radio did not - close the phone app that is using it and retry", name+"_BLE")
	}
	return "", fmt.Errorf("no Bluetooth device named %q found - is the printer switched on, in range and free (not used by the phone app)?", name)
}

// lookupCached matches the name in the paired list, then in every device BlueZ
// has seen, and reports which of the two matched. btctlOK says whether
// bluetoothctl could be run at all.
func lookupCached(name string) (mac string, paired, found, btctlOK bool) {
	for i, args := range [][]string{{"devices", "Paired"}, {"devices"}} {
		out, err := runBluetoothctl(args...)
		if err != nil {
			continue
		}
		btctlOK = true
		for _, line := range strings.Split(string(out), "\n") {
			// "Device 55:55:09:22:90:81 ML Printer"
			f := strings.Fields(line)
			if len(f) >= 3 && f[0] == "Device" && strings.EqualFold(strings.Join(f[2:], " "), name) {
				return f[1], i == 0, true, true
			}
		}
	}
	return "", false, false, btctlOK
}

// scan runs a short non-interactive discovery burst, so that a printer BlueZ
// has never seen can still be found (and remembered for next time).
func scan() {
	_, _ = runBluetoothctl("--timeout", "8", "scan", "on")
}

// runBluetoothctl runs bluetoothctl with a timeout: a broken bluetoothd makes
// it wait forever, even for something as simple as listing devices.
func runBluetoothctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "bluetoothctl", args...).Output()
}

// connect opens the SPP connection to the printer (with retries and messages).
func connect(addr, name string) (*sppConn, error) {
	conn, err := dialSPPWithRetry(addr, sppChannel, connectAttempts, connectTimeout)
	if err != nil {
		fmt.Printf("Connection failed: %v\n", err)
		if errors.Is(err, syscall.EBUSY) {
			fmt.Println("The printer is busy (connected to the phone app or to a stale link). Close the other connection and retry.")
		} else if errors.Is(err, syscall.EHOSTDOWN) {
			fmt.Println("The address is known but the printer does not answer - is it switched on and in range?")
		} else {
			fmt.Println("Use the Bluetooth Classic name (default \"ML Printer\"), not the *_BLE one.")
		}
		// Forget a remembered address only when the kernel says the ADDRESS is
		// bad. "Host is down" just means the printer is off (this one powers
		// itself off on a timer) and "busy" means something else is holding it -
		// neither says anything about the address, so the cache survives both,
		// and no scan is paid just because the printer was asleep.
		if name != "" && cachedAddr(name) == addr &&
			(errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENODEV)) {
			forgetAddr(name)
			fmt.Println("Forgot the address remembered for this printer; the next run will look it up again.")
		}
		return nil, err
	}
	return conn, nil
}

// linkAlive reports whether the printer is still at the other end of the socket.
// It peeks without consuming (a print in progress must still see every byte) and
// without transmitting, so it cannot be mistaken for activity and keep the
// printer awake. No data and no error means end of file: the link is gone.
func (c *sppConn) linkAlive() bool {
	var b [1]byte
	n, _, err := unix.Recvfrom(c.fd, b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
	if n > 0 {
		return true
	}
	if err != nil {
		return errors.Is(err, syscall.EAGAIN) ||
			errors.Is(err, syscall.EWOULDBLOCK) ||
			errors.Is(err, syscall.EINTR)
	}
	return false
}

// drain discards any pending input (e.g. a finished job's "OK") so that it is
// not mistaken for a status byte. It returns when no data arrives within the
// receive timeout.
func (c *sppConn) drain() {
	buf := make([]byte, 64)
	for {
		if n, err := unix.Read(c.fd, buf); err != nil || n == 0 {
			return
		}
	}
}

// query sends a command and returns the printer's whole answer (ok=false when
// it stays silent). It drains first, because after a job the printer sends "OK"
// - and 'O' (0x4F) has bit 2 set, so a leftover would look exactly like "no
// paper" in the status query.
func (c *sppConn) query(cmd []byte) ([]byte, bool) {
	_ = unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 120000})
	c.drain()
	if err := c.writeAll(cmd); err != nil {
		return nil, false
	}
	var out []byte
	buf := make([]byte, 128)
	for {
		n, err := unix.Read(c.fd, buf)
		if err != nil || n == 0 {
			break
		}
		out = append(out, buf[:n]...)
	}
	return out, len(out) > 0
}

// queryText is query() for the ASCII answers (model, firmware).
func (c *sppConn) queryText(cmd []byte) (string, bool) {
	b, ok := c.query(cmd)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(string(bytes.TrimRight(b, "\x00"))), true
}

// readStatus asks for the status byte ("10 FF 40").
func (c *sppConn) readStatus() (byte, bool) {
	b, ok := c.query([]byte{0x10, 0xff, 0x40})
	if !ok {
		return 0, false
	}
	return b[0], true
}

// batteryPercent reads the live battery level, or -1 when the printer is silent.
func batteryPercent(c *sppConn) int {
	b, ok := c.query([]byte{0x10, 0xff, 0x50, 0xf1})
	if !ok {
		return -1
	}
	return int(b[len(b)-1])
}

// batteryLevel formats that reading for -status and the session banner. Just the
// number. The firmware is not to be trusted about charging: there is no charging
// flag, and the same printer on the charger has been seen reporting 100% and its
// true level, so any comment would be a guess - and a wrong one.
func batteryLevel(c *sppConn) string {
	pct := batteryPercent(c)
	if pct < 0 {
		return ""
	}
	return fmt.Sprintf("%d%%", pct)
}

// autoOffMinutes reads the printer's auto power-off timer, in minutes, or -1
// when the printer does not answer.
func autoOffMinutes(c *sppConn) int {
	b, ok := c.query([]byte{0x10, 0xff, 0x13})
	if !ok {
		return -1
	}
	return int(b[len(b)-1])
}

// setAutoOff writes the printer's auto power-off timer, in minutes. The value is
// 16-bit big-endian and the printer acknowledges with "OK". It refuses 0 on
// purpose: per the community protocol notes, 0 does NOT disable the timer - it
// makes the printer switch itself off straight away.
func setAutoOff(c *sppConn, minutes int) error {
	if minutes <= 0 {
		return errors.New("refusing 0 minutes: it does not disable the timer, it powers the printer off at once")
	}
	cmd := []byte{0x10, 0xff, 0x12, byte(minutes >> 8), byte(minutes)}
	if _, ok := c.query(cmd); !ok {
		return errors.New("the printer did not acknowledge the auto-off command")
	}
	return nil
}

// statusError returns a human-readable message for a status bit that blocks
// printing, or "" when the printer is fine. Only "no paper" qualifies: the
// cover and head bits are dead on this model (see the const block above).
func statusError(st byte) string {
	if st&statusNoPaper != 0 {
		return "printer reports NO PAPER - load a new label roll"
	}
	return ""
}

// printText renders the text into a label, sends it, and waits for the printer
// to acknowledge the job before returning.
func printText(conn *sppConn, cfg config, text string) error {
	// Ask the printer first, and insist on an answer. With no paper it would
	// silently swallow the job and we would report success; and a link that has
	// just been released by another connection can look connected for a moment
	// and then swallow the job as well. Either way: an unanswered question means
	// we must not send the job. Saying so is better than wasting a label on a
	// link that is not really there.
	var st byte
	answered := false
	for attempt := 0; attempt < 3 && !answered; attempt++ {
		if attempt > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		st, answered = conn.readStatus()
	}
	if !answered {
		return errors.New("the printer did not answer - the link is not ready yet, try again in a few seconds")
	}
	if msg := statusError(st); msg != "" {
		return errors.New(msg)
	}
	job, _, err := renderLabel(cfg, text)
	if err != nil {
		return err
	}
	if err := conn.writeAll(job); err != nil {
		return err
	}
	// The printer sends "OK" when it is done - wait for that instead of a fixed
	// sleep, so success is reported as soon as it really finished (and the "OK"
	// is consumed, not left to confuse the next status query).
	if !conn.waitDone(10 * time.Second) {
		return errors.New("the printer did not acknowledge the job - it may have switched itself off")
	}
	return nil
}

// waitDone reads from the printer until the job is actually finished, i.e. until
// it sees "OK" a SECOND time, and reports whether that happened. The printer
// sends "OK" twice: once right after accepting the job (within ~15ms) and again
// when the print really completes. A timeout must NOT be taken for success: a
// job written to a printer that has switched itself off goes nowhere, and
// saying "printed" then would be a lie.
func (c *sppConn) waitDone(timeout time.Duration) bool {
	_ = unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 200000})
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 128)
	var seen []byte
	for time.Now().Before(deadline) {
		n, err := unix.Read(c.fd, buf)
		if err != nil || n == 0 {
			continue
		}
		seen = append(seen, buf[:n]...)
		if bytes.Count(seen, []byte("OK")) >= 2 {
			return true
		}
	}
	return false
}

// renderLabel draws the image and turns it into the printer's raster job. It
// also returns the raster height (for the wait time).
//
// The text grows to fill the label: after the rotation (rasterRotateDeg) its
// height lands on the print head, i.e. across the label (96 dots = 12 mm),
// and its length runs along the label.
//
// Two-line split (see -lines): the text is split at the best-balancing space if
// it has one, otherwise in the middle of the word, and the two-line layout is
// used only when it gives a clearly bigger font (twoLineMinGain). A hyphen '-'
// is the user's to type; there is no other special character.
// renderImage draws the text as the label bitmap, exactly as the printer would
// put it out: rotated to read along the tape, centred across the head and along
// the label. renderLabel turns this bitmap into bytes and -save-image writes it
// out as a PNG, so the picture and the print agree by construction.
func renderImage(cfg config, text string) (image.Image, error) {
	fontPath, err := resolveFont(cfg.fontFile)
	if err != nil {
		return nil, err
	}

	// Width from gg (its advances match what it draws), but height from the
	// font itself - gg reports a line height that is far too small.
	measure := func(s string, fs float64) (w, h float64, err error) {
		c := gg.NewContext(1, 1)
		if err := c.LoadFontFace(fontPath, fs); err != nil {
			return 0, 0, err
		}
		w, _ = c.MeasureString(s)
		ascent, descent, err := fontMetrics(fontPath, fs)
		if err != nil {
			return 0, 0, err
		}
		return w, ascent + descent, nil
	}

	lines := []string{text}
	fs := cfg.fontSize
	if fs <= 0 {
		fs = fitFont(cfg, lines, measure)
		if cfg.maxLines >= 2 {
			if a, b := splitTwo(text, measure); b != "" {
				if two := fitFont(cfg, []string{a, b}, measure); two > fs*twoLineMinGain {
					lines, fs = []string{a, b}, two
				}
			}
		}
	}

	// Measure at the final size.
	var maxW, lineH float64
	for _, ln := range lines {
		w, h, err := measure(ln, fs)
		if err != nil {
			return nil, fmt.Errorf("loading font: %w", err)
		}
		if w > maxW {
			maxW = w
		}
		if h > lineH {
			lineH = h
		}
	}
	if lineH <= 0 {
		lineH = fs
	}

	pad := 4.0
	cw := int(maxW + 2*pad)
	ch := int(float64(len(lines))*lineH + 2*pad)
	if cw < 1 {
		cw = 1
	}
	if ch < 1 {
		ch = 1
	}

	// Place the baseline one ascent below the top of each line box, so the ink
	// (ascender..descender) sits inside the box instead of hanging out of it.
	ascent, _, err := fontMetrics(fontPath, fs)
	if err != nil {
		return nil, fmt.Errorf("font metrics: %w", err)
	}

	dc := gg.NewContext(cw, ch)
	dc.SetColor(color.White)
	dc.Clear()
	dc.SetColor(color.Black)
	_ = dc.LoadFontFace(fontPath, fs)

	// Centre every line: horizontally on the label, vertically in its own box.
	for i, ln := range lines {
		y := pad + float64(i)*lineH + ascent
		dc.DrawStringAnchored(ln, float64(cw)/2, y, 0.5, 0)
	}

	// The text is laid out across the tape (readable), then turned so it reads
	// along it: see rasterRotateDeg. Centring on the head width then keeps it
	// from leaning left when the text does not fill 100% of the width.
	img := centerWidth(rotateImage(dc.Image(), rasterRotateDeg), cfg.width)

	// Also centre it ALONG the label: pad the raster up to the label length
	// (-len) so the text does not stick to the start.
	if cfg.labelLenMM > 0 {
		img = centerHeight(img, int(cfg.labelLenMM*8))
	}
	return img, nil
}

// renderLabel turns that bitmap into the byte stream the printer accepts.
func renderLabel(cfg config, text string) (job []byte, height int, err error) {
	img, err := renderImage(cfg, text)
	if err != nil {
		return nil, 0, err
	}
	raster, widthBytes, rasterHeight := imageToRaster(img)
	return buildJob(raster, widthBytes, rasterHeight, cfg.label), rasterHeight, nil
}

// addBorder returns a copy with a one-pixel black frame around it. Used for
// -save-image, so a mostly-white label keeps a visible extent when it is looked
// at on a white page. The printed bitmap never goes through this.
func addBorder(src image.Image) image.Image {
	b := src.Bounds()
	dc := gg.NewContext(b.Dx()+2, b.Dy()+2)
	dc.SetColor(color.Black)
	dc.Clear()
	dc.DrawImage(src, 1, 1)
	return dc.Image()
}

// runSaveImage writes the label out as a PNG in the current directory instead of
// printing it, so a layout can be checked without using tape - and without a
// printer, since nothing here talks to Bluetooth.
func runSaveImage(cfg config, text string, border bool, scale int) {
	name := imageName(text)
	img, err := renderImage(cfg, text)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	// The raster is turned so the text reads along the tape (rasterRotateDeg,
	// 90° clockwise), and a picture you look at on screen is easier with the
	// text horizontal: turn the same amount back. Only the .png is affected;
	// what gets printed is untouched.
	img = rotateImage(img, -rasterRotateDeg)
	// A label is mostly white, so on a white page in a viewer its edges simply
	// vanish. A one-pixel frame shows where the label is. Again: the picture
	// only, never the tape.
	if border {
		img = addBorder(img)
	}
	// Enlarge last, so the frame above stays one label dot thick instead of one
	// screen pixel - and nearest neighbour, so each dot remains a crisp square.
	img = scaleNearest(img, scale)

	if _, err := os.Stat(name); err == nil && !askOverwrite(name) {
		fmt.Println("Nothing written.")
		return
	}
	f, err := os.Create(name)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	b := img.Bounds()
	if scale > 1 {
		fmt.Printf("Wrote %s (%dx%d pixels, %dx the printed bitmap).\n", name, b.Dx(), b.Dy(), scale)
		return
	}
	fmt.Printf("Wrote %s (%dx%d dots, the exact bitmap that would be printed).\n", name, b.Dx(), b.Dy())
}

// scaleNearest enlarges by an integer factor, copying every dot into an n×n
// square. Nearest neighbour is deliberate: the label is a one-bit bitmap, so any
// smoothing would invent grey pixels that the printer cannot produce.
func scaleNearest(src image.Image, n int) image.Image {
	if n <= 1 {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx()*n, b.Dy()*n))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			for dy := 0; dy < n; dy++ {
				for dx := 0; dx < n; dx++ {
					dst.Set(x*n+dx, y*n+dy, c)
				}
			}
		}
	}
	return dst
}

// imageName turns the text into a file name: the phrase itself, plus .png. It
// stays in the current directory, so anything that would escape it (a slash, a
// control character, a leading dot) is replaced or dropped.
func imageName(text string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '-'
		case r < 32 || r == 127:
			return -1
		}
		return r
	}, text)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if name == "" {
		name = "label"
	}
	return name + ".png"
}

// askOverwrite asks before clobbering an existing file. Anything that is not a
// clear yes - including no answer at all - means no.
func askOverwrite(name string) bool {
	fmt.Printf("%s already exists. Overwrite it? [y/N] ", name)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// fontCandidates are tried before fontconfig is asked. DejaVu comes first
// because it is the font these labels have been sized and verified with, and the
// three paths are the usual Debian/Ubuntu, Fedora and Arch layouts. Everything
// else - another font, another layout, fonts kept in ~/.fonts - is fontconfig's
// job (see findFont).
var fontCandidates = []string{
	"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
	"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
}

// resolveFont returns the font file to render with: -fontfile if given,
// otherwise the first place on the system that has one (findFont). The result is
// resolved once per process, so a session does not pay for the lookup per label.
func resolveFont(explicit string) (string, error) {
	if explicit != "" {
		if err := loadFontFile(explicit); err != nil {
			return "", fmt.Errorf("cannot use -fontfile %s: %w", explicit, err)
		}
		return explicit, nil
	}
	fontOnce.Do(func() { fontPath, fontErr = findFont() })
	return fontPath, fontErr
}

var (
	fontOnce sync.Once
	fontPath string
	fontErr  error
)

// findFont looks for a bold font the way the system itself would. The known
// paths come first, so a machine that has DejaVu keeps printing the labels it
// always did; otherwise fontconfig is asked, which reads its own configuration
// and so covers whatever layout the distro uses plus fonts in ~/.fonts and
// ~/.local/share/fonts. DejaVu is an optional package on Debian and is absent
// from minimal installs, so this fallback is the difference between working and
// a hard-coded path that exists nowhere.
func findFont() (string, error) {
	for _, p := range fontCandidates {
		if loadFontFile(p) == nil {
			return p, nil
		}
	}
	// fontconfig lists every match best-first. We take the first that this tool
	// can actually read, because the answer may be a .ttc collection, which the
	// freetype parser rejects ("bad TTF version").
	if out, err := runFcMatch(); err == nil {
		for _, p := range strings.Split(string(out), "\n") {
			if p = strings.TrimSpace(p); p != "" && loadFontFile(p) == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("no bold font found on this system: the usual DejaVu paths are missing and fontconfig offered nothing readable - pass -fontfile PATH")
}

// runFcMatch asks fontconfig for every font matching a bold sans, best first.
// Like bluetoothctl, fc-match gets a timeout: a broken fontconfig should not
// hang the tool.
func runFcMatch() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "fc-match", "-a", "-f", "%{file}\n", "sans:bold").Output()
}

// loadFontFile reports whether the file is there and parses. It is the same
// check gg and fontMetrics make, so a font accepted here cannot fail later.
func loadFontFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = truetype.Parse(b)
	return err
}

// fontMetrics returns the font's REAL ascent and descent at this size. gg's
// MeasureString does not: at size 60 it reports a line height of 45 where the
// font actually needs ~70, so a box built from it is too short and clips every
// descender ("g" came out as "o" with a stub). These two numbers are what make
// the line box, and the baseline inside it, come out right.
func fontMetrics(fontPath string, size float64) (ascent, descent float64, err error) {
	b, err := os.ReadFile(fontPath)
	if err != nil {
		return 0, 0, err
	}
	f, err := truetype.Parse(b)
	if err != nil {
		return 0, 0, err
	}
	// Same options gg uses in LoadFontFace (DPI 72), so the sizes agree.
	face := truetype.NewFace(f, &truetype.Options{Size: size, DPI: 72})
	m := face.Metrics()
	return float64(m.Ascent) / 64, float64(m.Descent) / 64, nil
}

// fitFont finds the largest font size that fits the lines:
//   - across the head: the total height (lines x line height) takes up 90% of
//     the width,
//   - along the label: the widest line takes up 90% of the length
//     (203 dpi = 8 dots/mm); if labelLenMM = 0 there is no length limit.
func fitFont(cfg config, lines []string, measure func(string, float64) (float64, float64, error)) float64 {
	var maxW, h100 float64
	for _, ln := range lines {
		w, h, err := measure(ln, 100)
		if err != nil {
			continue
		}
		if w > maxW {
			maxW = w
		}
		if h > h100 {
			h100 = h
		}
	}
	if h100 <= 0 {
		return 32
	}
	fs := 0.9 * float64(cfg.width) * 100 / (float64(len(lines)) * h100)
	if cfg.labelLenMM > 0 && maxW > 0 {
		if lim := cfg.labelLenMM * 8 * 0.9 * 100 / maxW; lim < fs {
			fs = lim
		}
	}
	return fs
}

// splitTwo proposes splitting the text into two lines and returns the two
// lines. It splits at the best-balancing space if there is one, otherwise in the
// middle of the word (each line keeps at least minSplitRunes characters). We do
// NOT add a hyphen - the user types it (e.g. "MATH- EMATICS"). It returns
// ("", "") when no split is possible.
func splitTwo(text string, measure func(string, float64) (float64, float64, error)) (string, string) {
	src := []rune(text)
	if len(src) < 2 {
		return "", ""
	}

	var cands []int
	for i, r := range src {
		if r == ' ' && i > 0 && i < len(src)-1 {
			cands = append(cands, i+1) // after the space
		}
	}
	if len(cands) == 0 {
		// Each line must keep at least minSplitRunes characters.
		for i := minSplitRunes; i <= len(src)-minSplitRunes; i++ {
			cands = append(cands, i)
		}
		if len(cands) == 0 {
			return "", ""
		}
	}

	bestI, bestMax := -1, -1.0
	for _, i := range cands {
		a := strings.TrimSpace(string(src[:i]))
		b := strings.TrimSpace(string(src[i:]))
		w1, _, _ := measure(a, 100)
		w2, _, _ := measure(b, 100)
		m := w1
		if w2 > m {
			m = w2
		}
		if bestI < 0 || m < bestMax {
			bestI, bestMax = i, m
		}
	}
	a := strings.TrimSpace(string(src[:bestI]))
	b := strings.TrimSpace(string(src[bestI:]))
	if a == "" || b == "" {
		return "", ""
	}
	return a, b
}

// sppConn is a Bluetooth Classic RFCOMM (SPP) socket opened directly in the
// kernel, without bluez/bluetoothd.
type sppConn struct {
	fd int
}

// dialSPPWithRetry tries a few times, because connecting to these printers is
// occasionally unreliable: they may be asleep or still hold an old link.
func dialSPPWithRetry(displayAddr string, channel uint8, attempts int, timeout time.Duration) (*sppConn, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			fmt.Printf("  Retry %d/%d...\n", i+1, attempts)
			time.Sleep(connectRetryGap)
		}
		conn, err := dialSPP(displayAddr, channel, timeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		fmt.Printf("  Failed: %v\n", err)
	}
	return nil, lastErr
}

// dialSPP opens an RFCOMM (SPP) connection to the given Classic address.
//
// The RFCOMM framing (address/control/length/credit/FCS) is done by the kernel,
// so we just write the bare commands. Note: SockaddrRFCOMM.Addr wants the
// address in little-endian order - parseBDAddr returns it that way, whereas
// SockaddrL2 reverses it by itself.
func dialSPP(displayAddr string, channel uint8, timeout time.Duration) (*sppConn, error) {
	addr, err := parseBDAddr(displayAddr)
	if err != nil {
		return nil, err
	}

	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_STREAM, unix.BTPROTO_RFCOMM)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}

	// connect may block until the printer answers.
	done := make(chan error, 1)
	go func() {
		done <- unix.Connect(fd, &unix.SockaddrRFCOMM{Addr: addr, Channel: channel})
	}()
	select {
	case err := <-done:
		if err != nil {
			unix.Close(fd)
			return nil, err
		}
	case <-time.After(timeout):
		unix.Close(fd)
		return nil, fmt.Errorf("connect timeout (%s)", timeout)
	}

	// Timeouts so we never block forever.
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &unix.Timeval{Sec: 5})
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 5})

	return &sppConn{fd: fd}, nil
}

func (c *sppConn) Close() error {
	return unix.Close(c.fd)
}

// writeAll writes the whole stream to the SPP socket. The kernel splits it into
// RFCOMM frames (max N1 = 127 bytes of information) and adds addr/ctrl/length/
// credit/FCS - we do not build any framing ourselves.
func (c *sppConn) writeAll(b []byte) error {
	for len(b) > 0 {
		n, err := unix.Write(c.fd, b)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("write: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("zero-length write")
		}
		b = b[n:]
	}
	return nil
}

// parseBDAddr converts "AA:BB:CC:DD:EE:FF" into [6]byte in little-endian order,
// which is what SockaddrRFCOMM wants (the first byte of the printed form ends
// up last).
func parseBDAddr(s string) ([6]byte, error) {
	var addr [6]byte
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 6 {
		return addr, fmt.Errorf("invalid MAC address %q", s)
	}
	for i, part := range parts {
		v, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return addr, fmt.Errorf("invalid MAC address %q: %w", s, err)
		}
		addr[len(parts)-1-i] = byte(v)
	}
	return addr, nil
}

// buildJob frames the raster into the same sequence the official Android app
// uses ("confirmed working"):
// density -> wake -> enable -> GS v 0 -> [form feed | feed] -> stop job.
//
// With label=true it sends 1D 0C (form feed / "position next label") after the
// raster: that advances the paper to the next gap and aligns the next print.
// With label=false it feeds the paper plainly (1B 4A nn) for a continuous roll.
func buildJob(raster []byte, widthBytes, height int, label bool) []byte {
	var job []byte

	// 10 FF 10 00 nn - print density
	job = append(job, 0x10, 0xff, 0x10, 0x00, density)
	// 12 null bytes - wake. BEFORE the enable, like the official Android app's
	// "confirmed working" sequence. With enable-before-wake the printer reverses
	// and runs into the mechanical stop right at the start of the job.
	job = append(job, make([]byte, 12)...)
	// 10 FF F1 03 - enable printer (Lujiang mode = 3)
	job = append(job, cmdEnable...)

	// GS v 0 raster header + bitmap
	job = append(job, 0x1d, 0x76, 0x30, 0x00,
		byte(widthBytes%256), byte(widthBytes/256),
		byte(height%256), byte(height/256))
	job = append(job, raster...)

	if label {
		// 1D 0C - form feed / advance to the next label. THIS aligns.
		job = append(job, labelNext...)
		// 1B 4A 28 - the Android app feeds another 40 dots after the form feed.
		job = append(job, 0x1b, 0x4a, labelFeedDots)
	} else {
		// 1B 4A nn - paper feed
		job = append(job, 0x1b, 0x4a, feedDots)
	}
	// 10 FF F1 45 - stop job
	job = append(job, cmdStop...)
	return job
}

// rotateImage rotates the image by deg degrees (0/90/180/270), changing the
// dimensions where needed. It is called twice: 90° to turn the rendered text so
// it reads along the tape (rasterRotateDeg), and the same amount back for the
// .png that -save-image writes.
func rotateImage(src image.Image, deg int) image.Image {
	deg = ((deg % 360) + 360) % 360
	if deg == 0 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if deg == 90 || deg == 270 {
		nw, nh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			var nx, ny int
			switch deg {
			case 90:
				nx, ny = h-1-y, x
			case 180:
				nx, ny = w-1-x, h-1-y
			case 270:
				nx, ny = y, w-1-x
			default:
				nx, ny = x, y
			}
			dst.Set(nx, ny, c)
		}
	}
	return dst
}

// centerWidth places the image centred inside a canvas of width minWidth
// (dots), filling the rest with white. That way the raster covers the whole
// head width and the content does not stick to the left edge. If the image is
// already wider, it is returned as is.
func centerWidth(src image.Image, minWidth int) image.Image {
	b := src.Bounds()
	if b.Dx() >= minWidth {
		return src
	}
	dc := gg.NewContext(minWidth, b.Dy())
	dc.SetColor(color.White)
	dc.Clear()
	dc.DrawImage(src, (minWidth-b.Dx())/2, 0)
	return dc.Image()
}

// centerHeight does the same as centerWidth but for the height (label length):
// it centres the image inside a canvas of height minHeight, filling with white
// above and below. If it is already taller, it is returned as is.
func centerHeight(src image.Image, minHeight int) image.Image {
	b := src.Bounds()
	if b.Dy() >= minHeight {
		return src
	}
	dc := gg.NewContext(b.Dx(), minHeight)
	dc.SetColor(color.White)
	dc.Clear()
	dc.DrawImage(src, 0, (minHeight-b.Dy())/2)
	return dc.Image()
}

// imageToRaster converts the image into a 1-bit bitmap, MSB first, black = 1,
// and also returns the spans the GS v 0 header needs.
func imageToRaster(img image.Image) (data []byte, widthBytes, height int) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height = bounds.Dy()
	widthBytes = (width + 7) / 8

	data = make([]byte, 0, widthBytes*height)
	for y := 0; y < height; y++ {
		for xByte := 0; xByte < widthBytes; xByte++ {
			var currentByte byte
			for bit := 0; bit < 8; bit++ {
				x := xByte*8 + bit
				if x < width {
					r, g, b, _ := img.At(x, y).RGBA()
					luminance := (299*r + 587*g + 114*b) / 1000
					if luminance < 32768 { // black pixel
						currentByte |= 1 << (7 - uint(bit))
					}
				}
			}
			data = append(data, currentByte)
		}
	}
	return data, widthBytes, height
}

// lineReader reads lines from stdin with line editing and history
// (chzyer/readline). If readline cannot be initialised it falls back to plain
// reading.
type lineReader struct {
	rl  *readline.Instance
	br  *bufio.Reader
	old *unix.Termios // termios BEFORE readline, for restoring on hard exit
	fd  int
}

func newLineReader() *lineReader {
	lr := &lineReader{fd: int(os.Stdin.Fd())}
	// Keep the termios before readline enters raw mode: on timeout we exit with
	// os.Exit (defers do not run) and so we must restore it ourselves.
	if t, err := unix.IoctlGetTermios(lr.fd, unix.TCGETS); err == nil {
		lr.old = t
	}
	if rl, err := readline.NewEx(&readline.Config{
		Prompt:      "label> ",
		HistoryFile: historyFile(),
	}); err == nil {
		lr.rl = rl
		return lr
	}
	lr.br = bufio.NewReader(os.Stdin)
	return lr
}

func (l *lineReader) ReadLine() (string, error) {
	if l.rl != nil {
		return l.rl.Readline()
	}
	s, err := l.br.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// RestoreTerminal restores the terminal that readline left in raw mode. It is
// called before os.Exit (where defers do not run, so neither does Close). In
// the bufio case the terminal was never touched, so it does nothing.
func (l *lineReader) RestoreTerminal() {
	if l.rl != nil && l.old != nil {
		_ = unix.IoctlSetTermios(l.fd, unix.TCSETS, l.old)
	}
}

func (l *lineReader) Close() {
	if l.rl != nil {
		_ = l.rl.Close()
	}
}

// historyFile returns the history file in the user's home (empty string if no
// home is found - then we keep no history).
func historyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".label_history")
}
