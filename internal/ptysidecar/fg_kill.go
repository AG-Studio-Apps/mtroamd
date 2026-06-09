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
func killForegroundGroup(master *os.File, sessionPid int) error {
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
	// Negative pgid → signal the entire foreground group.
	return unix.Kill(-pgid, unix.SIGTERM)
}
