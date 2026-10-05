package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Remembering which address the printer was found at.
//
// This is a *cache*, not configuration: it is derived from what BlueZ reported,
// and deleting it costs one scan and nothing else. It exists because BlueZ
// itself forgets a printer that was merely discovered (a couple of minutes) and
// because pairing, which would make the address permanent, is awkward on this
// model: `bluetoothctl pair` reports a failure for its SPP-only handshake even
// when the device record is created, which just confuses people.
//
// The table is "@name<TAB>address" per line, so several names can be kept.

const cacheUsage = "usage: name<TAB>address"

// cacheFile is where the table lives. XDG_CACHE_HOME is honoured; "" means we
// have no home directory and the cache is simply disabled.
func cacheFile() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "label", "addr")
}

// cacheEntries reads the whole table. A missing or unreadable file is simply an
// empty table - a cache must never be able to break the program.
func cacheEntries() map[string]string {
	entries := map[string]string{}
	path := cacheFile()
	if path == "" {
		return entries
	}
	f, err := os.Open(path)
	if err != nil {
		return entries
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, mac, ok := strings.Cut(sc.Text(), "\t")
		if ok && name != "" && mac != "" {
			entries[name] = mac
		}
	}
	return entries
}

// cachedAddr returns the address remembered for this name, or "".
func cachedAddr(name string) string {
	return cacheEntries()[name]
}

// rememberAddr records name -> address, keeping the other entries.
func rememberAddr(name, mac string) {
	path := cacheFile()
	if path == "" || name == "" || mac == "" {
		return
	}
	entries := cacheEntries()
	if entries[name] == mac {
		return
	}
	entries[name] = mac
	writeEntries(path, entries)
}

// forgetAddr drops one entry, so the next run resolves the name afresh. Used
// when a remembered address stops working.
func forgetAddr(name string) {
	path := cacheFile()
	if path == "" {
		return
	}
	entries := cacheEntries()
	if _, ok := entries[name]; !ok {
		return
	}
	delete(entries, name)
	writeEntries(path, entries)
}

// writeEntries rewrites the table atomically, sorted so the file stays readable.
// Failures are ignored on purpose: not being able to cache is not an error.
func writeEntries(path string, entries map[string]string) {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte('\t')
		b.WriteString(entries[name])
		b.WriteByte('\n')
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
