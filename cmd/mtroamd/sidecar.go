package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/AG-Studio-Apps/mtroamd/internal/ptysidecar"
)

// runPtySidecar is the entry point for the `pty-sidecar` subcommand.
// The daemon spawns one of these per session; it owns the PTY master
// fd + child shell and forwards bytes to/from the daemon over a
// per-session Unix socket. See internal/ptysidecar for the design.
//
// Not invoked manually — flag parse failures still print usage so
// tooling can introspect via `mtroamd pty-sidecar --help`.
func runPtySidecar(args []string) int {
	fs := flag.NewFlagSet("pty-sidecar", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	socket := fs.String("socket", "", "path to bind for the per-session Unix socket (required)")
	pidfile := fs.String("pidfile", "", "path to write the flock'd pidfile (required)")
	sessionID := fs.String("session-id", "", "hex sessionID, used in log fields (optional)")
	shell := fs.String("shell", "", "absolute path to child shell ($SHELL → /bin/sh if empty)")
	var shellArgs stringSliceFlag
	fs.Var(&shellArgs, "shell-arg", "additional arg passed to the child shell after the binary name; repeat for multiple")
	rows := fs.Uint("rows", 24, "initial PTY rows")
	cols := fs.Uint("cols", 80, "initial PTY cols")
	envFile := fs.String("env-file", "", "path to KEY=VAL\\n env file; deleted by sidecar after read")
	graceSecs := fs.Int("grace-secs", ptysidecar.DefaultGraceSecs, "seconds to wait for daemon reconnect before reaping child")
	ringBytes := fs.Int("ring-bytes", ptysidecar.DefaultRingBytes, "capacity of the drop-oldest output ring")
	logPath := fs.String("log", "", "log file path; default stderr")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *socket == "" || *pidfile == "" {
		fmt.Fprintln(os.Stderr, "pty-sidecar: --socket and --pidfile are required")
		fs.Usage()
		return 2
	}
	if *rows > 0xFFFF || *cols > 0xFFFF {
		fmt.Fprintln(os.Stderr, "pty-sidecar: --rows and --cols must fit in uint16")
		return 2
	}

	logger := buildSidecarLogger(*logPath)

	// Memory safety: this sidecar owns a session's PTY + child shell, and every
	// process the user runs in that session is a descendant of it, so all of a
	// session's memory lives under this process tree. Raise our oom_score_adj so
	// that, under memory pressure, the kernel OOM-killer sacrifices a runaway
	// SESSION rather than the mtroamd daemon (small, default score → never the
	// victim) or other, lower-memory sessions. The child shell inherits this
	// score, so the whole session subtree is the preferred victim. RAISING is
	// unprivileged (a negative/protective adjust would need CAP_SYS_RESOURCE a
	// user systemd service lacks); memory usage dominates the badness score, so
	// the session that ballooned is the one picked. Best-effort, Linux-only.
	makeSessionOOMPreferred(logger)

	cfg := ptysidecar.Config{
		SocketPath:  *socket,
		PidfilePath: *pidfile,
		SessionID:   *sessionID,
		Shell:       *shell,
		ShellArgs:   shellArgs,
		Rows:        uint16(*rows),
		Cols:        uint16(*cols),
		EnvFile:     *envFile,
		GraceSecs:   *graceSecs,
		RingBytes:   *ringBytes,
		Logger:      logger,
	}

	// SIGTERM/SIGINT handling lives inside Run; we hand it a
	// cancellable context so the daemon can also tear us down by
	// closing the socket and waiting.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := ptysidecar.Run(ctx, cfg); err != nil {
		logger.Error("sidecar.run_failed", "err", err.Error())
		return 1
	}
	return 0
}

// sessionOOMScoreBump is how far ABOVE the daemon's score we push each session.
// The sidecar inherits the daemon's oom_score_adj at fork; we read that and add
// this, so a runaway session is the OOM victim regardless of the inherited
// baseline. This is RELATIVE, not absolute, because a systemd USER manager
// defaults its services (and thus mtroamd + every session) to a non-zero
// oom_score_adj (observed 200 on Ubuntu): a fixed absolute below that baseline
// would leave sessions LESS killable than the daemon (backwards). Range is
// -1000..1000; +300 clears any realistic default while leaving headroom.
const (
	sessionOOMScoreBump = 300
	oomScoreAdjPath     = "/proc/self/oom_score_adj"
	oomScoreAdjMax      = 1000
)

// makeSessionOOMPreferred raises this sidecar's oom_score_adj (inherited by the
// child shell + everything the user runs) strictly above the daemon's, so a
// memory-hungry session is the OOM victim, never mtroamd. RAISING is unprivileged
// (a protective lower/negative would need CAP_SYS_RESOURCE a user service lacks).
// Best-effort + Linux-only.
//
// We ONLY proceed on a successful read of the current score: target =
// inherited+bump is then always a raise from the live value, so we never attempt
// to LOWER (which is privileged and would fail with EPERM). Guessing a 0 baseline
// on a read failure could compute a below-baseline target on a host whose real
// score exceeds the bump, so we skip instead.
func makeSessionOOMPreferred(logger *slog.Logger) {
	b, err := os.ReadFile(oomScoreAdjPath)
	if err != nil {
		logger.Debug("could not read oom_score_adj (skipping)", "err", err.Error())
		return
	}
	inherited, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		logger.Debug("could not parse oom_score_adj (skipping)", "value", string(b))
		return
	}
	target := inherited + sessionOOMScoreBump
	if target > oomScoreAdjMax {
		target = oomScoreAdjMax
	}
	if target <= inherited {
		return // already at the ceiling; nothing to raise
	}
	if err := os.WriteFile(oomScoreAdjPath, []byte(strconv.Itoa(target)), 0o644); err != nil {
		logger.Debug("could not raise session oom_score_adj", "err", err.Error())
		return
	}
	logger.Debug("session oom_score_adj raised", "from", inherited, "to", target)
}

// stringSliceFlag is a flag.Value that accumulates repeated --flag=val
// occurrences into a slice. Used for --shell-arg.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%v", []string(*s))
}

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func buildSidecarLogger(path string) *slog.Logger {
	var sink io.Writer = os.Stderr
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			sink = f
		}
	}
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
