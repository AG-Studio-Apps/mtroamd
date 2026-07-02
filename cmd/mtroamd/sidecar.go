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
	// process the user runs in that session is a descendant of it — so all of a
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

// sessionOOMScoreAdj is the oom_score_adj we set on each pty-sidecar. A modest
// positive value (range is -1000..1000, default 0) biases the kernel OOM-killer
// toward reaping a runaway session over the daemon and over other, lower-memory
// processes on the box — the daemon's sessions should give way first. Memory
// usage still dominates the badness score, so a low-memory session isn't picked
// just for carrying this adjustment.
const sessionOOMScoreAdj = "100"

// makeSessionOOMPreferred raises this sidecar's oom_score_adj (inherited by the
// child shell + everything the user runs) so a memory-hungry session is the OOM
// victim, never mtroamd. Best-effort: the file is Linux-only and the write may
// be denied in constrained sandboxes — a failure just leaves the default score,
// which is still safer than the pre-fix state (no swap + uncapped cgroup).
func makeSessionOOMPreferred(logger *slog.Logger) {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(sessionOOMScoreAdj), 0o644); err != nil {
		logger.Debug("could not raise session oom_score_adj", "err", err.Error())
		return
	}
	logger.Debug("session oom_score_adj raised", "value", sessionOOMScoreAdj)
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
