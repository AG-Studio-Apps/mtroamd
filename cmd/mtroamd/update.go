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

	"github.com/AG-Studio-Apps/mtroamd/internal/build"
	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/release"
	"github.com/AG-Studio-Apps/mtroamd/internal/svcmgr"
)

// runUpdate implements `mtroamd update [--check] [--yes] [--tag X]`.
//
// What it does:
//  1. Resolve the target tag (latest or --tag).
//  2. If the current build matches the target, exit 0 with "up to date".
//  3. Download SHA256SUMS + SHA256SUMS.minisig + the platform binary.
//  4. Verify SHA256SUMS minisig signature against the embedded
//     primary + emergency public-key roster (same as iOS).
//  5. Verify SHA-256 of the downloaded binary against the entry in
//     the now-trusted SHA256SUMS.
//  6. Atomic-replace the running binary.
//  7. Restart the daemon via the detected supervisor.
//
// Exit codes:
//   0  up to date OR update succeeded
//   1  update available (only when --check is passed)
//   2  bad flags / user cancelled
//   3  verification failed
//   4  download / network failure
//   5  service restart failed (binary updated but daemon may be down)
func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false,
		"print current vs available version and exit. "+
			"Exit 0 if up to date, 1 if an update is available, "+
			"3 on verification failure, 4 on network error.")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	tag := fs.String("tag", "", "update to a specific tag instead of the latest release")
	allowDowngrade := fs.Bool("allow-downgrade", false,
		"permit installing a tag older than the running version. "+
			"By default a downgrade refuses to proceed — even with --tag — "+
			"so an accidental or attacker-induced 'latest' rollback can't "+
			"silently install a known-vulnerable older signed build.")
	force := fs.Bool("force", false,
		"reinstall the target tag even if it matches/precedes the running "+
			"version (bypasses the version checks; signature + checksum "+
			"verification still apply). Useful to re-pull a re-cut tag or to "+
			"move between same-base prereleases.")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: mtroamd update [flags]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// Refuse to self-update a package-managed install (apt/dpkg, AUR,
	// Homebrew). Writing the new binary to ~/.local/bin would leave a
	// shadow copy that diverges from what the package manager tracks and
	// (on a --user host) silently rewrite the unit to point at it. `--check`
	// is still allowed — reporting the available version is harmless.
	if release.IsPackageManaged() && !*checkOnly {
		fmt.Fprintf(os.Stderr, "update: %s\n",
			release.PackageManagedHint("sudo apt upgrade mtroamd"))
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fetcher := release.NewFetcher()
	current := build.Version

	target := *tag
	if target == "" {
		var err error
		target, _, err = fetcher.LatestTag(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "update: %v\n", err)
			return 4
		}
	}
	if !strings.HasPrefix(target, "v") {
		target = "v" + target
	}
	if err := release.ValidateTag(target); err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 2
	}

	fmt.Printf("current:    %s\n", current)
	fmt.Printf("available:  %s\n", target)

	if release.VersionsMatch(current, target) {
		if !*force {
			fmt.Println("✓ already on this version")
			return 0
		}
		fmt.Printf("▸ --force: reinstalling %s (already on this version)\n", target)
	}

	// Anti-rollback: refuse to silently install a tag older than the
	// running version. The minisign signature on an older release is
	// still valid (it was signed at the time) — so without this check
	// a flipped GitHub "latest" pointer, a malicious mirror, or a
	// typoed `--tag` could downgrade us into a known-bad build.
	// --force bypasses this ordering check (crypto verification below
	// is NOT bypassed); --allow-downgrade narrowly permits a downgrade.
	cmp, ok := release.CompareSemver(target, current)
	if ok && cmp < 0 {
		if !*allowDowngrade && !*force {
			fmt.Fprintf(os.Stderr,
				"update: refusing to downgrade %s → %s. "+
					"Re-run with --allow-downgrade if this is intentional.\n",
				current, target)
			return 3
		}
		fmt.Printf("▸ downgrading %s → %s (forced)\n", current, target)
	}

	if *checkOnly {
		fmt.Println("Update available. Run `mtroamd update` to apply.")
		return 1
	}

	if !*yes {
		fmt.Print("\nApply update? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			fmt.Println("Cancelled.")
			return 2
		}
	}

	return performUpdate(ctx, fetcher, target)
}

// performUpdate runs the full verify-and-swap pipeline. Split out so
// `runUpdate` stays under the function-length lint ceiling.
func performUpdate(ctx context.Context, fetcher *release.Fetcher, tag string) int {
	asset, err := release.AssetFilename()
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 4
	}
	binPath := release.JoinBin()
	destDir := filepath.Dir(binPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "update: create bin dir: %v\n", err)
		return 4
	}

	fmt.Println("▸ downloading signed checksums")
	shaSumsURL := fetcher.AssetURL(tag, "SHA256SUMS")
	sigURL := fetcher.AssetURL(tag, "SHA256SUMS.minisig")
	shaSums, err := fetcher.FetchSmall(ctx, shaSumsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 4
	}
	sigFile, err := fetcher.FetchSmall(ctx, sigURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 4
	}

	fmt.Println("▸ verifying signature")
	roster, err := release.TrustedRoster()
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: roster: %v\n", err)
		return 3
	}
	result, err := release.MinisignVerify(shaSums, sigFile, roster)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: signature: %v\n", err)
		return 3
	}
	// Bind the signature to the requested tag — minisign verifies the
	// SHA list was signed by our key, but not WHICH release it belongs
	// to. Without this, a re-published older SHA256SUMS would still
	// verify and the anti-rollback semver check would still accept the
	// "newer" target, letting an older (potentially vulnerable) binary
	// in. See internal/release/trusted_comment.go for the contract.
	if err := release.EnforceTrustedComment(result.TrustedComment, tag); err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 3
	}
	fmt.Printf("  signed by key %d (%s)\n", result.KeyIndex, result.TrustedComment)
	if result.KeyIndex == 1 {
		fmt.Fprintln(os.Stderr,
			"  ! EMERGENCY signing key was used — verify this release with the maintainer")
	}

	fmt.Printf("▸ downloading %s\n", asset)
	tmpPath, err := fetcher.FetchBinary(ctx, fetcher.AssetURL(tag, asset), destDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 4
	}
	defer os.Remove(tmpPath) // best-effort; rename below moves it on success

	fmt.Println("▸ verifying binary hash")
	expected, err := release.LookupChecksum(shaSums, asset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 3
	}
	actual, err := release.ChecksumOf(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 3
	}
	if actual != expected {
		fmt.Fprintf(os.Stderr, "update: SHA-256 mismatch (expected %s, got %s)\n",
			expected, actual)
		return 3
	}

	fmt.Println("▸ swapping binary")
	if err := release.AtomicReplace(tmpPath, binPath, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "update: replace: %v\n", err)
		return 4
	}

	fmt.Println("▸ restarting daemon")
	mgr := svcmgr.Detect(ctx)
	if err := mgr.Restart(ctx, binPath); err != nil {
		fmt.Fprintf(os.Stderr, "update: restart via %s: %v\n", mgr.Name(), err)
		fmt.Fprintln(os.Stderr,
			"  binary is updated; restart the daemon manually to pick it up")
		return 5
	}

	// "Installed" must mean "SERVING": a botched earlier install left a
	// stale daemon holding the listeners while the fresh binary sat idle
	// - this command printed success, every client connect failed, and
	// nothing named the cause (2026-07-21 incident). So ask the LIVE
	// daemon what version it runs and refuse to report success until it
	// matches the tag we just installed.
	fmt.Println("▸ verifying the running daemon")
	if code := verifyRunningDaemon(ctx, tag, binPath); code != 0 {
		return code
	}

	fmt.Printf("\n✓ Updated to %s via %s (running daemon verified)\n", tag, mgr.Name())
	return 0
}

// versionToken reduces a daemon-reported version to its comparable
// tag: Status carries build.String() — "v1.7.8-rc2 (sha, built …)" —
// and release.VersionsMatch on the FULL string never matches a bare
// tag (caught in pre-release testing: every update would have failed
// its own verification). First whitespace-separated field only.
func versionToken(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

// verifyRunningDaemon polls the daemon's Status IPC until it reports
// the freshly-installed tag (the supervisor restart is asynchronous;
// systemd needs a moment to bring the new process up), then checks the
// serve-process census for stragglers. Returns 0 on verified success,
// 5 (the "restart failed" class) otherwise - the binary IS updated,
// but the host is not actually serving it.
func verifyRunningDaemon(ctx context.Context, tag, binPath string) int {
	socketPath := discoverClientSocketPath()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	var lastVersion string
	for time.Now().Before(deadline) && ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		status, err := ipc.NewClient(socketPath, 2*time.Second).Status(probeCtx)
		cancel()
		switch {
		case err != nil:
			lastErr = err
		case !status.Ok:
			lastErr = fmt.Errorf("daemon not ok: %s", status.Msg)
		case release.VersionsMatch(versionToken(status.Version), tag):
			fmt.Printf("  running daemon reports %s\n", status.Version)
			// The live daemon is the new one; now make sure no STALE
			// serve process lingers alongside it (it could still hold
			// the QUIC/TCP listener ports the clients dial).
			var stale []string
			for _, p := range findServeProcesses() {
				if p.Deleted || (p.Exe != "" && p.Exe != binPath) {
					stale = append(stale, fmt.Sprintf("pid %d (%s)", p.PID, p.Exe))
				}
			}
			if len(stale) > 0 {
				fmt.Fprintf(os.Stderr,
					"update: STALE serve process(es) still running: %s\n"+
						"  they may hold the client-facing listeners - kill them "+
						"and re-run `mtroamd doctor`\n",
					strings.Join(stale, ", "))
				return 5
			}
			return 0
		default:
			// A daemon answered but with the WRONG version - either the
			// old process is still draining its restart, or a stale one
			// owns the socket. Keep polling until the deadline decides.
			lastVersion = status.Version
			lastErr = nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastVersion != "" {
		fmt.Fprintf(os.Stderr,
			"update: binary is %s but the RUNNING daemon still reports %s - "+
				"a stale process is serving; run `mtroamd doctor` to see the "+
				"serve-process census, kill the stray, and restart\n",
			tag, lastVersion)
	} else {
		fmt.Fprintf(os.Stderr,
			"update: binary updated but no daemon answered at %s within 15s "+
				"(last error: %v) - start it manually and run `mtroamd doctor`\n",
			socketPath, lastErr)
	}
	return 5
}

// Version comparison helpers moved to internal/release/version.go so
// mtroam's update path can share them. See release.VersionsMatch and
// release.CompareSemver.
