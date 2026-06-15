//go:build linux || darwin

package ptysidecar

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// killForegroundGroup SIGTERMs the PTY's foreground process group
// (kill(-pgid)) — the agent plus any children it spawned — while the
// session's shell, which job control keeps in a DIFFERENT process
// group, survives and returns to its prompt. Foundation for the
// kill-and-resume restart. The mechanism is POSIX (tcgetpgrp + getsid
// + kill), so Linux and macOS share this one implementation.
//
// sessionPid getsid-confines the signal exactly like the comm/cwd
// resolvers: a recycled foreground pgid — one that exited between the
// TIOCGPGRP and the kill and was reassigned to an unrelated process —
// must never be signalled, or we'd SIGTERM a stranger's process group.
// sessionPid<=0 disables the confinement.
// expectAgent (when non-empty) is the foreground command the daemon believed was
// running when it decided to recover. We re-read the LIVE foreground command for the
// SAME pgid right before signalling and refuse the SIGTERM unless it still matches —
// so a foreground that changed to a non-agent (the agent exited, a different program
// took the tty) between the daemon's idle-gate decision and this kill is never
// signalled. (H1, security audit v1.7.0. Residual: a tool running as a CHILD in the
// agent's OWN process group still reads as the agent — the idle gate is the only
// guard there; see recover.go.)
func killForegroundGroup(master *os.File, sessionPid int, expectAgent string) error {
	pgid, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return err
	}
	if pgid <= 0 {
		return fmt.Errorf("killForegroundGroup: no foreground process group")
	}
	if sessionPid > 0 {
		if sid, serr := unix.Getsid(pgid); serr != nil || sid != sessionPid {
			return fmt.Errorf("killForegroundGroup: foreground pgid %d not in session %d", pgid, sessionPid)
		}
	}
	if expectAgent != "" {
		if comm := ResolveForegroundComm(pgid, sessionPid); comm != expectAgent {
			return fmt.Errorf("killForegroundGroup: live foreground %q != expected agent %q — refusing SIGTERM", comm, expectAgent)
		}
	}
	// Negative pgid → signal the entire foreground group.
	return unix.Kill(-pgid, unix.SIGTERM)
}
