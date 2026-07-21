//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// findServeProcesses enumerates every `mtroamd serve` process this uid
// can see via /proc. Born from the 2026-07-21 incident: a botched
// install/uninstall left a STALE daemon running (old binary, holding
// the listeners) while the freshly-installed binary never served —
// the installer reported success, every connect failed, and nothing
// on the host named the problem. The census makes that state visible:
// doctor warns on multiples / deleted-exe processes, and update refuses
// to report success while one lingers.
func findServeProcesses() []serveProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []serveProcess
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue // vanished or not ours to read
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(argv) < 2 || argv[1] != "serve" ||
			filepath.Base(argv[0]) != "mtroamd" {
			continue
		}
		if pid == os.Getpid() {
			continue
		}
		p := serveProcess{PID: pid}
		// Readlink works for same-uid processes; a deleted target keeps
		// serving the OLD code — the exact stale-daemon signature.
		if exe, err := os.Readlink(filepath.Join("/proc", ent.Name(), "exe")); err == nil {
			p.Exe = strings.TrimSuffix(exe, " (deleted)")
			p.Deleted = strings.HasSuffix(exe, " (deleted)")
		}
		out = append(out, p)
	}
	return out
}
