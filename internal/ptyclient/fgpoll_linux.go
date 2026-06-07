//go:build linux

package ptyclient

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Linux leaf lookups for the daemon-side fg fallback (the loop lives in
// fgpoll.go). The shell's tpgid (field 8 of /proc/<pid>/stat) is the
// controlling terminal's foreground process group — what tcgetpgrp
// returns — so no PTY master is needed.

// sessionShellPID finds the sidecar's direct child (the shell) by
// scanning /proc for ppid==sidecarPID. Called once per session.
// Relies on the sidecar having exactly one direct child (the shell
// from cpty.StartWithSize); returns the first match. If the sidecar
// ever forks additional direct children this would need to
// disambiguate by comm.
func sessionShellPID(sidecarPID int) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-pid entry (cpuinfo, self, …)
		}
		if ppid, ok := ppidOf(pid); ok && ppid == sidecarPID {
			return pid, true
		}
	}
	return 0, false
}

// ppidOf returns /proc/<pid>/stat field 4 (ppid).
func ppidOf(pid int) (int, bool) {
	f, ok := statAfterComm(pid)
	if !ok || len(f) < 2 {
		return 0, false
	}
	v, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, false
	}
	return v, true
}

// tpgidOf returns /proc/<pid>/stat field 8 (tpgid) — the foreground
// process group of the controlling terminal.
func tpgidOf(pid int) (int, bool) {
	f, ok := statAfterComm(pid)
	if !ok || len(f) < 6 {
		return 0, false
	}
	v, err := strconv.Atoi(f[5])
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// statAfterComm reads /proc/<pid>/stat and returns the whitespace-
// separated fields starting at field 3 (state) — i.e. everything after
// the comm field. comm (field 2) is wrapped in parens and may itself
// contain spaces and parens, so we split on the LAST ')': field 3 is
// index 0, ppid (f4) is index 1, tpgid (f8) is index 5.
func statAfterComm(pid int) ([]string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, false
	}
	return parseStatAfterComm(string(data))
}

// parseStatAfterComm is the pure parser behind statAfterComm (split out
// for testing the comm-in-parens gotcha). Splits on the LAST ')' so a
// comm value that itself contains spaces and/or parens — e.g.
// "(a) b)" — doesn't shift the field indices.
func parseStatAfterComm(s string) ([]string, bool) {
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 || rp+1 >= len(s) {
		return nil, false
	}
	return strings.Fields(s[rp+1:]), true
}
