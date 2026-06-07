package ptyclient

import (
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/ptysidecar"
)

// Daemon-side foreground-command fallback for sessions whose sidecar
// never self-reports fg (pre-1.6.x sidecars that a 1.6.x daemon
// reconnected to after an upgrade — KillMode=process spares them).
// The daemon owns no PTY master, but the sidecar's child shell is the
// controlling-terminal session leader, so the shell's tpgid IS that
// terminal's foreground process group — the same value tcgetpgrp
// returns. The result feeds the same fgVal cache the FrameFgState path
// writes, so the carried-over session shows fg in `list` + the iOS
// capsule without anyone killing it.
//
// This loop is platform-agnostic; only the two leaf lookups
// (sessionShellPID, tpgidOf) are per-OS — /proc on Linux (fgpoll_linux.go),
// sysctl on macOS (fgpoll_darwin.go), stubbed elsewhere (fgpoll_other.go).
const (
	// fgFallbackGrace lets a live 1.6.x sidecar send its first
	// FrameFgState (5s tick) before the daemon does any work; if it
	// does, the poller stands down with zero cost.
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
	// restart. Stub leaves (unsupported OS) return false here → idle.
	shell, ok := sessionShellPID(c.sidecarPID)
	if !ok {
		c.logger.Debug("fg-fallback: child shell not found; poller idle",
			"sidecar_pid", c.sidecarPID)
		return
	}
	c.shellPID = shell
	c.logger.Debug("fg-fallback: resolving fg (pre-1.6.x sidecar)",
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
				c.logger.Debug("fg-fallback: resolved fg",
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

// foregroundFromProc reads the cached shell's tpgid (per-OS leaf) and
// resolves it through the SAME getsid-confined resolver the sidecar
// uses. c.shellPID is touched only by the single poller goroutine
// (write-once before the loop, read-only here), so no lock is needed.
func (c *Conn) foregroundFromProc() string {
	tpgid, ok := tpgidOf(c.shellPID)
	if !ok {
		return ""
	}
	return ptysidecar.ResolveForegroundComm(tpgid, c.shellPID)
}
