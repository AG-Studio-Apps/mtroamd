package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/build"
	"github.com/AG-Studio-Apps/mtroamd/internal/release"
	"github.com/AG-Studio-Apps/mtroamd/internal/svcmgr"
)

// doctorFixKind is a remediation `mtroamd doctor --fix` can attempt.
type doctorFixKind int

const (
	fixStartDaemon     doctorFixKind = iota // daemon down; a supervisor can start it
	fixRestartDaemon                        // stale/skewed serve process; supervisor restart + stray sweep
	fixRefreshUnit                          // unit file missing or lacks KillMode=process (systemd-user)
	fixEnableLinger                         // linger disabled (systemd-user, Linux)
	adviseNoSupervisor                      // daemon down/stale but backend is nohup / unreachable — report only
)

type plannedFix struct {
	kind doctorFixKind
	why  string
}

// planFixes maps a diagnosed report to the ordered remediations
// doctor --fix should attempt. It is PURE and table-tested: every host
// fact is passed in (backend + availability from the detected Manager,
// selfVersion = the running binary's build.Version, goos for the linger
// gate). Only a SUPERVISED backend (systemd-user / launchd, reachable)
// gets a live action for daemon liveness; nohup / unreachable gets an
// advisory instead of an auto-spawn — mirroring connect's "never
// nohup-spawn a listener the operator didn't choose" restraint.
//
// Ordering: daemon liveness first (a down or stale daemon is the
// headline failure), then unit hygiene, then linger.
func planFixes(r DoctorReport, backend string, available bool, goos, selfVersion string) []plannedFix {
	var out []plannedFix
	supervised := available && backend != "nohup"

	switch {
	case !r.Daemon.Running:
		if supervised {
			out = append(out, plannedFix{fixStartDaemon, "daemon not running"})
		} else {
			out = append(out, plannedFix{adviseNoSupervisor, "daemon not running"})
		}
	case staleServe(r, selfVersion):
		reason := staleServeReason(r, selfVersion)
		if supervised {
			out = append(out, plannedFix{fixRestartDaemon, reason})
		} else {
			out = append(out, plannedFix{adviseNoSupervisor, reason})
		}
	}

	if backend == "systemd-user" && r.UnitFile != nil && (!r.UnitFile.Present || !r.UnitFile.KillModeProcess) {
		out = append(out, plannedFix{fixRefreshUnit, "unit file missing or lacks KillMode=process"})
	}
	if backend == "systemd-user" && goos == "linux" &&
		r.Linger != nil && !r.Linger.Enabled && r.Linger.Error == "" {
		out = append(out, plannedFix{fixEnableLinger, "linger disabled - sessions die at logout"})
	}
	return out
}

// staleServe reports the botched-update signature: more than one serve
// process, one running a DELETED binary, or the running daemon's version
// predating this binary. Mirrors buildDoctorReport's warning conditions
// so --fix acts on exactly what the report flags.
func staleServe(r DoctorReport, selfVersion string) bool {
	if len(r.Processes) > 1 {
		return true
	}
	for _, p := range r.Processes {
		if p.Deleted {
			return true
		}
	}
	return runningVersionSkew(r, selfVersion)
}

// runningVersionSkew is the "the serving process predates this binary"
// check, gated (like the report's) off an un-ldflagged dev build.
func runningVersionSkew(r DoctorReport, selfVersion string) bool {
	return r.Daemon.Running && r.Daemon.Version != "" && selfVersion != "v0.0.0-dev" &&
		!release.VersionsMatch(versionToken(r.Daemon.Version), selfVersion)
}

func staleServeReason(r DoctorReport, selfVersion string) string {
	switch {
	case len(r.Processes) > 1:
		return fmt.Sprintf("%d serve processes running", len(r.Processes))
	case runningVersionSkew(r, selfVersion):
		return fmt.Sprintf("running daemon %s predates this binary %s", r.Daemon.Version, selfVersion)
	default:
		return "a serve process is running a deleted binary"
	}
}

// runDoctorFix applies the planned remediations after the report has
// already been printed. Returns the process exit code: 0 if the box
// ends healthy, doctorExitWarnings otherwise. Execution is thin and
// host-dependent; the decision logic it drives lives in planFixes.
func runDoctorFix(ctx context.Context, r DoctorReport, socketPath string, timeout time.Duration) int {
	mgr := svcmgr.Detect(ctx)
	plan := planFixes(r, mgr.Name(), mgr.Available(ctx), runtime.GOOS, build.Version)
	if len(plan) == 0 {
		if len(r.Warnings) == 0 {
			fmt.Println("\n✓ Nothing to fix.")
			return doctorExitOK
		}
		fmt.Println("\nNo auto-fixable warnings (see the report above); nothing changed.")
		return doctorExitWarnings
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mtroamd doctor --fix: can't resolve own path: %v\n", err)
		return doctorExitWarnings
	}
	tag := versionToken(build.Version)

	fmt.Println("\nApplying fixes:")
	daemonTouched := false
	for _, f := range plan {
		switch f.kind {
		case adviseNoSupervisor:
			fmt.Printf("  • %s: backend is %s - no reachable supervisor to restart safely.\n"+
				"      SIGTERM the stale serve pid(s) shown above and re-run your `mtroamd serve` command.\n",
				f.why, mgr.Name())
		case fixStartDaemon:
			fmt.Printf("  • starting daemon via %s (%s)…\n", mgr.Name(), f.why)
			if err := mgr.Start(ctx, binPath); err != nil {
				fmt.Printf("      ✘ start failed: %v\n", err)
			} else {
				daemonTouched = true
			}
		case fixRestartDaemon:
			fmt.Printf("  • restarting daemon via %s (%s)…\n", mgr.Name(), f.why)
			if err := mgr.Restart(ctx, binPath); err != nil {
				fmt.Printf("      ✘ restart failed: %v\n", err)
			} else {
				daemonTouched = true
				if killed := sweepStaleServe(binPath); len(killed) > 0 {
					fmt.Printf("      swept stale serve pid(s): %v\n", killed)
				}
			}
		case fixRefreshUnit:
			fmt.Printf("  • refreshing unit file (%s)…\n", f.why)
			if err := refreshUnitFile(ctx, mgr); err != nil {
				fmt.Printf("      ✘ %v\n", err)
			}
		case fixEnableLinger:
			fmt.Printf("  • enabling linger (%s)…\n", f.why)
			if err := enableLinger(ctx); err != nil {
				fmt.Printf("      ✘ %v\n", err)
			}
		}
	}

	// After any daemon liveness action, refuse to claim success until the
	// LIVE daemon reports the installed tag with no stale straggler - the
	// same "installed must mean serving" gate `mtroamd update` enforces.
	if daemonTouched {
		fmt.Println("\n▸ verifying the running daemon")
		if code := verifyRunningDaemon(ctx, tag, binPath); code != 0 {
			return code
		}
	}

	fmt.Println("\n▸ re-checking")
	after := buildDoctorReport(socketPath, timeout)
	if len(after.Warnings) == 0 {
		fmt.Println("✓ All checks pass after fixes.")
		return doctorExitOK
	}
	fmt.Printf("✘ %d warning(s) remain:\n", len(after.Warnings))
	for _, w := range after.Warnings {
		fmt.Printf("  - %s\n", w)
	}
	return doctorExitWarnings
}

// sweepStaleServe SIGTERMs same-uid serve processes carrying POSITIVE
// stale evidence (a deleted exe, or a resolved path other than the
// installed binary) that survive a restart as untracked orphans. The
// fresh daemon (exe == the installed binary, not deleted) is never a
// target, so this can't kill the process we just started - unlike a
// blanket `pkill -x mtroamd`. Returns the pids signalled. No-op on
// non-Linux, where findServeProcesses returns nothing.
func sweepStaleServe(binPath string) []int {
	resolved := binPath
	if r, err := filepath.EvalSymlinks(binPath); err == nil {
		resolved = r
	}
	self := os.Getpid()
	var killed []int
	for _, p := range findServeProcesses() {
		if p.PID == self {
			continue
		}
		if p.Deleted || (p.Exe != "" && p.Exe != resolved) {
			if proc, err := os.FindProcess(p.PID); err == nil && proc.Signal(syscall.SIGTERM) == nil {
				killed = append(killed, p.PID)
			}
		}
	}
	return killed
}

// enableLinger runs `loginctl enable-linger <user>` so a systemd-user
// daemon survives logout/reboot. Idempotent (enabling twice is fine).
func enableLinger(ctx context.Context) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "loginctl", "enable-linger", u.Username).CombinedOutput()
	if err != nil {
		return fmt.Errorf("loginctl enable-linger %s: %v: %s", u.Username, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// refreshUnitFile rewrites the systemd-user unit from the current
// binary's renderer (the same output as `mtroamd unit print`) and
// reloads the manager, fixing a missing file or a stale one lacking
// KillMode=process. 0644 matches the render's world-readable-by-design
// unit (no secrets).
func refreshUnitFile(ctx context.Context, mgr svcmgr.Manager) error {
	path := mgr.UnitPath()
	if path == "" {
		return fmt.Errorf("no unit path for backend %s", mgr.Name())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir unit dir: %w", err)
	}
	var buf bytes.Buffer
	if code := runUnitPrint(nil, &buf); code != 0 {
		return fmt.Errorf("render unit failed (exit %d)", code)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", path, err)
	}
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
	return nil
}
