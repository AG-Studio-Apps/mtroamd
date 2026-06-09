//go:build darwin

package ptysidecar

import (
	"os"

	"golang.org/x/sys/unix"
)

// macOS foreground-command resolution. Mirrors fg_linux.go but maps a
// PID to a name via sysctl (KERN_PROC / KERN_PROCARGS2) instead of
// /proc, which macOS lacks. Pure-Go (no cgo) so the daemon's static
// CGO_ENABLED=0 build is unaffected. The confinement (getsid) and the
// tcgetpgrp foreground-group read are POSIX, shared in spirit with the
// Linux build; the selection + sanitize logic is shared via
// fg_common.go (commFromArgs/capComm/parseProcArgs).

// foregroundComm: tcgetpgrp on the master (portable) → ResolveForegroundComm.
func foregroundComm(master *os.File, sessionPid int) string {
	pgid, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return ""
	}
	return ResolveForegroundComm(pgid, sessionPid)
}

// ResolveForegroundComm turns a foreground pgid into a bare command
// name, getsid-confined to sessionPid and UTF-8-capped. comm comes
// from kinfo_proc.P_comm; when that's empty or an interpreter
// ("node"-wrapped CLIs — the common Claude/Codex case), fall back to
// the argv[0]/script basename from KERN_PROCARGS2. sessionPid<=0
// disables confinement.
func ResolveForegroundComm(pgid, sessionPid int) string {
	if pgid <= 0 {
		return ""
	}
	if sessionPid > 0 {
		if sid, serr := unix.Getsid(pgid); serr != nil || sid != sessionPid {
			return ""
		}
	}
	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pgid); err == nil && kp != nil {
		name := commFromCString(kp.Proc.P_comm[:])
		if name != "" && !isInterpreterComm(name) {
			return capComm(name)
		}
	}
	// comm missing or an interpreter — argv fallback (same selection
	// logic as Linux's /proc/<pid>/cmdline path, different source).
	if name := commFromArgs(procArgs(pgid)); name != "" {
		return capComm(name)
	}
	return ""
}

// procArgs reads a process's argv via KERN_PROCARGS2 and decodes it.
// Same-uid-only by the kernel (targets are the user's own processes;
// the daemon runs as the user). Returns nil on any error → caller
// degrades to "" (no fg), never a panic.
func procArgs(pid int) [][]byte {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) == 0 {
		return nil
	}
	return parseProcArgs(buf)
}

// foregroundCwd is a stub on macOS — resolving a foreground process's
// cwd needs libproc (proc_pidinfo PROC_PIDVNODEPATHINFO), which the
// pure-Go / CGO_ENABLED=0 build can't reach today. "" = unknown, so
// FrameFgCwd carries nothing and the restart's cwd stays empty. The
// killForegroundGroup mechanism (fg_kill.go) is POSIX and works here.
// v1.6.2+; macOS cwd support is future work.
func foregroundCwd(_ *os.File, _ int) string { return "" }
