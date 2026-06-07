//go:build darwin

package ptyclient

import "golang.org/x/sys/unix"

// macOS leaf lookups for the daemon-side fg fallback (the loop lives in
// fgpoll.go). kinfo_proc gives ppid and the controlling-terminal
// foreground pgrp (Tpgid) directly via sysctl — no /proc, no /stat
// parsing. Pure-Go (no cgo).

// sessionShellPID finds the sidecar's direct child (the shell) by
// scanning the process table for Eproc.Ppid==sidecarPID. Called once
// per session. Same single-direct-child assumption as the Linux leaf.
func sessionShellPID(sidecarPID int) (int, bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, false
	}
	for i := range procs {
		if int(procs[i].Eproc.Ppid) == sidecarPID {
			return int(procs[i].Proc.P_pid), true
		}
	}
	return 0, false
}

// tpgidOf returns the controlling terminal's foreground process group
// for pid (kinfo_proc Eproc.Tpgid) — the macOS equivalent of Linux's
// /proc/<pid>/stat field 8.
func tpgidOf(pid int) (int, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	tpgid := int(kp.Eproc.Tpgid)
	if tpgid <= 0 {
		return 0, false
	}
	return tpgid, true
}
