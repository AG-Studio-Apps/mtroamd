package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AG-Studio-Apps/meshtermd/internal/release"
	"github.com/AG-Studio-Apps/meshtermd/internal/svcmgr"
)

// runMigrate implements `meshtermd migrate` — switch a host's systemd --user
// setup from a prior ~/.local/bin install (placed by the iOS app or
// `meshtermd update`) onto the package-managed binary this command is running
// as (e.g. /usr/bin/meshtermd from the apt package).
//
// State in ~/.local/share/meshtermd (cert, sessions/) is shared by both
// installs — same user, same $HOME — so nothing is copied and the iOS
// cert-pin survives. This is purely an install-topology cutover:
//  1. rewrite the user unit's ExecStart to point at the new binary, keeping
//     the old unit's --addr/--socket/--roam-tcp-addr flags so the daemon
//     binds the same endpoints the iOS app dials;
//  2. daemon-reload + restart — KillMode=process keeps live PTY sessions, the
//     new daemon reattaches the surviving sidecars;
//  3. ONLY after confirming the new daemon is active, remove the stale
//     ~/.local/bin binary. ~/.local/share is never touched.
//
// Safe order + reversible: on any failure before the new daemon is confirmed
// active, the original unit is restored and the old binary left intact.
// Idempotent: a no-op when there's nothing to migrate. Run as the login user
// (no sudo — it only READS the new binary path); the apt postinstall invokes
// it for $SUDO_USER when their user bus is reachable, else prints the command.
//
// Exit codes:
//
//	0  migrated, already migrated, or nothing to migrate
//	2  bad flags / cancelled / not the package-managed binary
//	5  cutover failed (original unit restored; old install left intact)
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: meshtermd migrate [--yes]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// We migrate TO the binary this process is running as — which must be the
	// package-managed install. Running the ~/.local/bin binary means there's
	// nothing to migrate to.
	newBin := release.RunningBinaryPath()
	if !release.IsPackageManaged() {
		fmt.Println("migrate: this is the ~/.local/bin install — nothing to migrate to.")
		fmt.Println("Run migrate from the package-managed binary, e.g. `/usr/bin/meshtermd migrate`.")
		return 0
	}

	oldBin := release.JoinBin() // ~/.local/bin/meshtermd
	unitPath := svcmgr.UserUnitPath()
	oldUnit, unitErr := os.ReadFile(unitPath)
	haveOldUnit := unitErr == nil
	haveOldBin := fileExists(oldBin)

	if !haveOldUnit && !haveOldBin {
		fmt.Println("migrate: no prior ~/.local/bin install detected — nothing to do.")
		return 0
	}
	// Already migrated: unit points at the new binary and the old binary is gone.
	if haveOldUnit && strings.Contains(string(oldUnit), "ExecStart="+newBin+" ") && !haveOldBin {
		fmt.Println("migrate: already migrated to", newBin)
		return 0
	}

	if !*yes {
		fmt.Println("Migrate this host's meshtermd to the package-managed binary:")
		fmt.Printf("  • %s → %s\n", oldBin, newBin)
		fmt.Println("  • rewrites the systemd --user unit + restarts (live sessions preserved)")
		fmt.Println("  • removes the old binary; shared state in ~/.local/share/meshtermd is kept")
		fmt.Print("\nProceed? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			fmt.Println("Cancelled.")
			return 2
		}
	}

	// Build the new unit, preserving the old unit's bind flags if present.
	opts := svcmgr.UserUnitOptions{BinPath: newBin}
	if haveOldUnit {
		parseExecStartFlags(string(oldUnit), &opts)
	}
	newUnit := svcmgr.RenderUserUnit(&opts)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: create unit dir: %v\n", err)
		return 5
	}
	if err := os.WriteFile(unitPath, []byte(newUnit), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: write unit: %v\n", err)
		return 5
	}

	// Restore the pre-migration state if the cutover fails — leave the user on
	// a working old install rather than a half-migrated one.
	rollback := func(stage string, out []byte, err error) int {
		fmt.Fprintf(os.Stderr, "migrate: %s: %v (%s) — restoring previous install\n",
			stage, err, strings.TrimSpace(string(out)))
		if haveOldUnit {
			_ = os.WriteFile(unitPath, oldUnit, 0o644)
		} else {
			_ = os.Remove(unitPath)
		}
		_, _ = svcmgr.SystemctlUser(ctx, "daemon-reload")
		_, _ = svcmgr.SystemctlUser(ctx, "restart", "meshtermd")
		return 5
	}

	if out, err := svcmgr.SystemctlUser(ctx, "daemon-reload"); err != nil {
		return rollback("daemon-reload", out, err)
	}
	// enable --now is idempotent for an already-enabled unit and also covers
	// the rare "old binary but unit not enabled" path.
	if out, err := svcmgr.SystemctlUser(ctx, "enable", "--now", "meshtermd"); err != nil {
		return rollback("enable --now", out, err)
	}
	// Force a restart: enable --now won't restart an already-active unit, so
	// without this an already-running daemon would keep executing the OLD
	// (~/.local/bin) binary despite the rewritten ExecStart.
	if out, err := svcmgr.SystemctlUser(ctx, "restart", "meshtermd"); err != nil {
		return rollback("restart", out, err)
	}
	if out, err := svcmgr.SystemctlUser(ctx, "is-active", "meshtermd"); err != nil ||
		strings.TrimSpace(string(out)) != "active" {
		return rollback("verify is-active", out, err)
	}

	// Cutover confirmed active — now safe to remove the stale binary.
	if haveOldBin && oldBin != newBin {
		if err := os.Remove(oldBin); err != nil {
			fmt.Fprintf(os.Stderr, "migrate: note: couldn't remove old binary %s: %v\n", oldBin, err)
		} else {
			fmt.Printf("▸ removed old binary %s\n", oldBin)
		}
	}
	fmt.Printf("✓ migrated to %s — sessions preserved. Updates now via: sudo apt upgrade meshtermd\n", newBin)
	return 0
}

// parseExecStartFlags pulls --addr/--socket/--roam-tcp-addr out of the old
// unit's ExecStart line into opts, so the rewritten unit binds the same
// endpoints. A missing flag leaves the field empty (RenderUserUnit defaults);
// an absent --roam-tcp-addr is preserved as the "-" (QUIC-only) sentinel.
func parseExecStartFlags(unit string, opts *svcmgr.UserUnitOptions) {
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		toks := strings.Fields(line)
		sawTCP := false
		for i := 0; i+1 < len(toks); i++ {
			switch toks[i] {
			case "--addr":
				opts.Addr = toks[i+1]
			case "--socket":
				opts.SocketPath = toks[i+1]
			case "--roam-tcp-addr":
				opts.TCPAddr = toks[i+1]
				sawTCP = true
			}
		}
		if !sawTCP {
			opts.TCPAddr = "-"
		}
		return
	}
}
