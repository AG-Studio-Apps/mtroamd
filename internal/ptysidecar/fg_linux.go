//go:build linux

package ptysidecar

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// foregroundComm returns the command name of the PTY's current
// foreground process group — the same mechanism tmux uses for
// pane_current_command: tcgetpgrp on the master fd, then the group
// leader's /proc/<pgid>/comm (falling back to the cmdline argv[0]
// basename for interpreter wrappers whose comm is unhelpful, e.g.
// "node"-shebanged CLIs).
//
// Returns "" when unresolvable: fd error, no foreground group, the
// leader already exited, or /proc unreadable. Privacy: this reads
// process NAMES only — never arguments beyond the argv[0] basename,
// never terminal content.
//
// sessionPid is the sidecar's shell child — the controlling-terminal
// session leader (creack/pty starts it with Setsid+Setctty). It
// confines the /proc read to this PTY's own session: between
// TIOCGPGRP and the /proc read the foreground pgid could exit and be
// recycled by an unrelated process, and a bare /proc/<pgid>/comm
// would then leak that stranger's name to the client. Verifying
// getsid(pgid) == sessionPid rejects any recycled pid outside our
// session. A zero sessionPid disables the check.
func foregroundComm(master *os.File, sessionPid int) string {
	pgid, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return ""
	}
	return ResolveForegroundComm(pgid, sessionPid)
}

// ResolveForegroundComm turns a foreground process-group id into a
// bare command name, getsid-confined to sessionPid and UTF-8-capped.
// Two callers obtain the pgid differently: the sidecar via tcgetpgrp
// on the PTY master (foregroundComm above), and the DAEMON via
// /proc/<shell>/stat tpgid (the carryover-session fallback for
// pre-1.6.x sidecars). One resolver → the confinement + sanitization
// can't drift. sessionPid<=0 disables the getsid confinement.
// (Darwin has its own build of this in fg_darwin.go; the selection
// logic — commFromArgs/capComm — is shared via fg_common.go.)
func ResolveForegroundComm(pgid, sessionPid int) string {
	if pgid <= 0 {
		return ""
	}
	if sessionPid > 0 {
		if sid, serr := unix.Getsid(pgid); serr != nil || sid != sessionPid {
			return ""
		}
	}
	// Group leader's pid == pgid under shell job control.
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgid))
	if err == nil {
		name := strings.TrimSpace(string(comm))
		if name != "" && !isInterpreterComm(name) {
			return capComm(name)
		}
	}
	// comm missing or an interpreter name — try the argv[0]/script
	// basename so "node /usr/local/bin/claude" classifies as "claude".
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pgid))
	if err != nil || len(cmdline) == 0 {
		return ""
	}
	if name := commFromArgs(bytes.Split(cmdline, []byte{0})); name != "" {
		return capComm(name)
	}
	return ""
}
