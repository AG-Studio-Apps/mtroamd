package main

import (
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
	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/release"
	"github.com/AG-Studio-Apps/mtroamd/internal/svcmgr"
)

// doctorFixKind is a step `mtroamd doctor --fix` can take. Some are live
// remediations; others are advisories for the cases --fix must NOT act on
// automatically (needs privilege, needs the operator's config, or would
// cost live sessions).
type doctorFixKind int

const (
	fixStartDaemon         doctorFixKind = iota // daemon down; supervisor start (verify sweeps strays)
	fixRestartDaemon                            // stale daemon, restart is session-safe (KillMode=process or launchd)
	fixEnableLinger                             // linger disabled; attempt, then advise sudo on a denial
	adviseNoSupervisor                          // down/stale but nohup / unreachable — SIGTERM + re-serve manually
	adviseUnitRefresh                           // unit missing / lacks KillMode=process — `mtroamd unit print`
	adviseRestartNeedsUnit                      // stale daemon, but a restart would drop sessions (no KillMode) — refresh unit first
)

type plannedFix struct {
	kind doctorFixKind
	why  string
}

// serveCensusSuspicious is the census-only stale signal: more than one
// serve process, or one on a deleted binary. Shared by buildDoctorReport's
// dampened re-sample and by the fix planner so the report and --fix can't
// drift on what "stale" means.
func serveCensusSuspicious(ps []serveProcess) bool {
	if len(ps) > 1 {
		return true
	}
	for _, p := range ps {
		if p.Deleted {
			return true
		}
	}
	return false
}

// runningVersionSkew reports whether a running daemon's reported version
// predates this binary, gated off an un-ldflagged dev build (that compare
// is meaningless and would cry wolf on every dev run). Shared by the
// report warning and the planner.
func runningVersionSkew(daemonVer, selfVer string) bool {
	return daemonVer != "" && selfVer != "v0.0.0-dev" &&
		!release.VersionsMatch(versionToken(daemonVer), selfVer)
}

// planFixes maps a diagnosed report to the ordered steps doctor --fix
// should take. PURE and table-tested: every host fact is passed in
// (backend + availability from the detected Manager, selfVersion = this
// binary's build.Version, goos for the linger gate).
//
// Guardrails baked into the plan, not just the executor:
//   - Only a SUPERVISED backend (systemd-user / launchd, reachable) gets a
//     live start/restart; nohup / unreachable gets an advisory, never an
//     auto-spawn (connect's "never nohup-spawn an unconsented listener").
//   - A restart is planned only when it is SESSION-SAFE: if the systemd
//     unit lacks KillMode=process, restarting wipes the cgroup and drops
//     every session, so the plan advises refreshing the unit first instead.
//   - The unit is never auto-rewritten (it can carry custom ExecStart
//     flags we can't reconstruct) — it is always an advisory.
func planFixes(r DoctorReport, backend string, available bool, goos, selfVersion string) []plannedFix {
	var out []plannedFix
	supervised := available && backend != "nohup"
	unitBad := backend == "systemd-user" && r.UnitFile != nil && (!r.UnitFile.Present || !r.UnitFile.KillModeProcess)
	unitCoveredByRestartAdvice := false

	switch {
	case !r.Daemon.Running:
		if supervised {
			out = append(out, plannedFix{fixStartDaemon, "daemon not running"})
		} else {
			out = append(out, plannedFix{adviseNoSupervisor, "daemon not running"})
		}
	case staleServe(r, selfVersion):
		reason := staleServeReason(r, selfVersion)
		switch {
		case !supervised:
			out = append(out, plannedFix{adviseNoSupervisor, reason})
		case unitBad:
			// A restart here would drop every session (no KillMode=process).
			// Advise the unit refresh first; the operator re-runs --fix.
			out = append(out, plannedFix{adviseRestartNeedsUnit, reason})
			unitCoveredByRestartAdvice = true
		default:
			out = append(out, plannedFix{fixRestartDaemon, reason})
		}
	}

	if unitBad && !unitCoveredByRestartAdvice {
		out = append(out, plannedFix{adviseUnitRefresh, unitRefreshWhy(r)})
	}
	if backend == "systemd-user" && goos == "linux" &&
		r.Linger != nil && !r.Linger.Enabled && r.Linger.Error == "" {
		out = append(out, plannedFix{fixEnableLinger, "linger disabled - sessions die at logout"})
	}
	return out
}

// staleServe is the botched-update signature the report warns on: a
// suspicious census (multiple / deleted-exe) or a running daemon whose
// version predates this binary. Built from the shared predicates so it
// stays in lockstep with buildDoctorReport.
func staleServe(r DoctorReport, selfVersion string) bool {
	return serveCensusSuspicious(r.Processes) ||
		(r.Daemon.Running && runningVersionSkew(r.Daemon.Version, selfVersion))
}

func staleServeReason(r DoctorReport, selfVersion string) string {
	switch {
	case len(r.Processes) > 1:
		return fmt.Sprintf("%d serve processes running", len(r.Processes))
	case r.Daemon.Running && runningVersionSkew(r.Daemon.Version, selfVersion):
		return fmt.Sprintf("running daemon %s predates this binary %s", r.Daemon.Version, selfVersion)
	default:
		return "a serve process is running a deleted binary"
	}
}

func unitRefreshWhy(r DoctorReport) string {
	if r.UnitFile != nil && !r.UnitFile.Present {
		return "unit file missing"
	}
	return "unit lacks KillMode=process (sessions drop on restart)"
}

// runDoctorFix applies the planned steps after the report has printed.
// Returns doctor's exit code: 0 if the box ends healthy, doctorExitWarnings
// otherwise (never a foreign code — the doctor contract is 0/1/2). The
// decision logic lives in planFixes; this is thin execution.
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

	// The supervisor launches the daemon from the unit's ExecStart (the
	// INSTALLED path), NOT from wherever doctor happens to run - so all
	// stale-vs-fresh reasoning keys on the install path, never os.Executable.
	binPath := release.JoinBin()
	tag := versionToken(build.Version)
	unitPath := mgr.UnitPath()

	fmt.Println("\nApplying fixes:")
	daemonTouched := false
	for _, f := range plan {
		switch f.kind {
		case adviseNoSupervisor:
			fmt.Printf("  • %s: backend is %s - no reachable supervisor to (re)start safely.\n"+
				"      SIGTERM the stale serve pid(s) shown above and re-run your `mtroamd serve` command.\n",
				f.why, mgr.Name())
		case adviseUnitRefresh:
			fmt.Printf("  • %s - run: mtroamd unit print > %s   (preserving your flags), then systemctl --user daemon-reload\n",
				f.why, unitPath)
		case adviseRestartNeedsUnit:
			fmt.Printf("  • stale daemon (%s), but a restart would drop ALL sessions: the unit lacks KillMode=process.\n"+
				"      refresh it first: mtroamd unit print > %s  (preserving your flags), systemctl --user daemon-reload,\n"+
				"      then re-run `mtroamd doctor --fix` to restart safely.\n", f.why, unitPath)
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
			}
		case fixEnableLinger:
			fmt.Printf("  • enabling linger (%s)…\n", f.why)
			switch outcome, uname, err := enableLinger(ctx); outcome {
			case lingerEnabled:
				fmt.Println("      ✓ linger enabled")
			case lingerNeedsSudo:
				fmt.Printf("      needs privilege - run: sudo loginctl enable-linger %s\n", uname)
			default:
				fmt.Printf("      ✘ %v\n", err)
			}
		}
	}

	// After any liveness action, refuse to claim success until the LIVE
	// daemon reports the installed tag - the "installed must mean serving"
	// gate. verifyDaemonServing sweeps strays and polls; a timeout is a
	// warning (exit 1), never a foreign exit code.
	if daemonTouched {
		fmt.Println("\n▸ verifying the running daemon")
		if !verifyDaemonServing(ctx, tag) {
			return doctorExitWarnings
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

// verifyDaemonServing sweeps stale strays, then polls the live daemon
// until it reports `tag` (matching update's post-restart gate) or a 30s
// deadline passes. Doctor-scoped messaging - update.verifyRunningDaemon
// prints "update:"-prefixed text telling the user to re-run doctor, wrong
// here. Sweep FIRST so a stray holding a listener can't block the fresh
// daemon from binding its ports.
func verifyDaemonServing(ctx context.Context, tag string) bool {
	if killed := sweepStaleServe(release.JoinBin()); len(killed) > 0 {
		fmt.Printf("      swept stale serve pid(s): %v\n", killed)
	}
	sockets := []string{discoverClientSocketPath()}
	if p := persistentSocketPath(); p != sockets[0] {
		sockets = append(sockets, p)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		for _, sp := range sockets {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			status, err := ipc.NewClient(sp, 2*time.Second).Status(probeCtx)
			cancel()
			if err == nil && status.Ok && release.VersionsMatch(versionToken(status.Version), tag) {
				fmt.Printf("      ✓ running daemon reports %s\n", tag)
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("      ✘ no daemon reported %s within 30s — run `mtroamd doctor` for detail\n", tag)
	return false
}

// sweepStaleServe SIGTERMs same-uid serve processes carrying POSITIVE
// stale evidence (a deleted exe, or a resolved path other than the
// INSTALLED binary) that survive as untracked orphans. servedBin is the
// install path (release.JoinBin), which is what the supervisor launches -
// so the fresh daemon (exe == servedBin, not deleted) is never a target,
// unlike a blanket `pkill -x mtroamd`. Returns the pids signalled. No-op
// on non-Linux, where findServeProcesses returns nothing.
func sweepStaleServe(servedBin string) []int {
	resolved := servedBin
	if r, err := filepath.EvalSymlinks(servedBin); err == nil {
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

// lingerOutcome is the result of attempting `loginctl enable-linger`.
// Unlike the daemon restart / unit steps (all user-scoped), enable-linger
// writes system state (/var/lib/systemd/linger) and normally needs root or
// a polkit admin prompt - and doctor runs UNPRIVILEGED. So we attempt it
// (systemd >=248 can allow self-linger on an active session) and, on the
// common permission denial, hand back a sudo advisory instead of an error.
type lingerOutcome int

const (
	lingerEnabled   lingerOutcome = iota // succeeded (unprivileged self-linger path worked)
	lingerNeedsSudo                      // denied by polkit/permissions — advise `sudo loginctl enable-linger`
	lingerFailed                         // some other failure (tooling missing, etc.)
)

// enableLinger runs `loginctl enable-linger <user>`. Idempotent. Returns
// the outcome, the username (for the advisory), and the raw error only for
// lingerFailed.
func enableLinger(ctx context.Context) (lingerOutcome, string, error) {
	u, err := user.Current()
	if err != nil {
		return lingerFailed, "", err
	}
	out, runErr := exec.CommandContext(ctx, "loginctl", "enable-linger", u.Username).CombinedOutput()
	if runErr == nil {
		return lingerEnabled, u.Username, nil
	}
	msg := strings.TrimSpace(string(out))
	if lingerNeedsPrivilege(msg) {
		return lingerNeedsSudo, u.Username, nil
	}
	return lingerFailed, u.Username, fmt.Errorf("loginctl enable-linger %s: %v: %s", u.Username, runErr, msg)
}

// lingerNeedsPrivilege reports whether loginctl's failure output is a
// polkit / permission denial (the case that needs root) rather than a
// genuine tooling failure. Matches the phrasings polkit and systemd emit
// when there is no way to authenticate (no agent on a headless SSH login).
func lingerNeedsPrivilege(out string) bool {
	l := strings.ToLower(out)
	for _, s := range []string{
		"interactive authentication required",
		"access denied",
		"permission denied",
		"not authorized",
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
