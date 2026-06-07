//go:build linux

package ptyclient

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/ptysidecar"
)

// Daemon-side foreground-command fallback for sessions whose sidecar
// never self-reports fg (pre-1.6.x sidecars that a 1.6.x daemon
// reconnected to after an upgrade — KillMode=process spares them).
// Instead of the sidecar's tcgetpgrp-on-master path, the daemon
// resolves fg entirely from /proc: it owns no PTY master, but the
// sidecar's child shell is the controlling-terminal session leader, so
// /proc/<shell>/stat field 8 (tpgid) IS that terminal's foreground
// process group — the same value tcgetpgrp returns. The result feeds
// the same fgVal cache the FrameFgState path writes, so the carried-
// over session shows fg in `list` + the iOS capsule without anyone
// killing it. Linux-only (the sidecar fg path is too).
const (
	// fgFallbackGrace lets a live 1.6.x sidecar send its first
	// FrameFgState (5s tick) before the daemon does any /proc work; if
	// it does, the poller stands down with zero cost.
	fgFallbackGrace = 7 * time.Second
	// fgFallbackInterval matches the sidecar poll cadence.
	fgFallbackInterval = 5 * time.Second
)

func (c *Conn) runFgFallbackPoller() {
	if c.sidecarPID <= 0 {
		return // unknown sidecar pid → can't resolve; safe no-op.
	}
	select {
	case <-c.readerDone:
		return
	case <-time.After(fgFallbackGrace):
	}
	if c.fgIsCapable() {
		return // sidecar self-reports — nothing for us to do.
	}
	// Resolve the sidecar's child shell once (stable for the session's
	// life). ppid==sidecarPID holds even for sidecars we reconnected
	// to: the shell's parent is the sidecar, untouched by a daemon
	// restart.
	shell, ok := sessionShellPID(c.sidecarPID)
	if !ok {
		c.logger.Debug("fg-fallback: child shell not found; poller idle",
			"sidecar_pid", c.sidecarPID)
		return
	}
	c.shellPID = shell
	c.logger.Debug("fg-fallback: resolving fg from /proc (pre-1.6.x sidecar)",
		"sidecar_pid", c.sidecarPID, "shell_pid", shell)

	ticker := time.NewTicker(fgFallbackInterval)
	defer ticker.Stop()
	logged := false // log the first successful resolution once (triage).
	for {
		if c.fgIsCapable() {
			return // a FrameFgState arrived late — sidecar takes over.
		}
		if comm := c.foregroundFromProc(); comm != "" {
			c.fgMu.Lock()
			// Never override the sidecar once it proves capable; only
			// publish a genuine change.
			if !c.fgCapable && c.fgVal != comm {
				c.fgVal = comm
			}
			c.fgMu.Unlock()
			if !logged {
				logged = true
				c.logger.Debug("fg-fallback: resolved fg from /proc",
					"shell_pid", c.shellPID, "fg", comm)
			}
		}
		select {
		case <-c.readerDone:
			return
		case <-ticker.C:
		}
	}
}

func (c *Conn) fgIsCapable() bool {
	c.fgMu.Lock()
	defer c.fgMu.Unlock()
	return c.fgCapable
}

// foregroundFromProc reads the cached shell's tpgid and resolves it
// through the SAME getsid-confined resolver the sidecar uses.
// c.shellPID is touched only by the single poller goroutine
// (write-once before the loop, read-only here), so no lock is needed.
func (c *Conn) foregroundFromProc() string {
	tpgid, ok := tpgidOf(c.shellPID)
	if !ok {
		return ""
	}
	return ptysidecar.ResolveForegroundComm(tpgid, c.shellPID)
}

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
