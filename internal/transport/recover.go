package transport

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// defaultGrace is the upper bound on the IDLE-GATE wait when the client
// doesn't specify one — how long we'll wait for the agent's turn to
// finish before giving up (NOT a forced exit; see runRecover). 30s is
// generous: a long tool call (build, test run) keeps the agent's
// spinner animating, so we wait it out rather than interrupt it.
const defaultGrace = 30 * time.Second

// maxGrace caps GraceMillis from the client. 2 minutes is more than
// enough for any idle wait; uncapped values would let a stolen token
// hold goroutines + timers indefinitely.
const maxGrace = 2 * time.Minute

// idleQuietWindow is how long the PTY output must stay silent before we
// treat the agent's turn as complete. Claude/Codex animate a status
// spinner the entire time a turn is in flight — INCLUDING while a tool
// (a shell command, a build) runs — so a quiet stream is the reliable
// "back at the input prompt, nothing running" signal. This is the
// load-bearing safety gate: it's what stops us interrupting an
// in-flight `git commit` / `npm` and corrupting a repo. 1.5s comfortably
// outlasts the spinner's redraw cadence (~80-120ms) without adding
// noticeable latency to a genuinely-idle restart.
const idleQuietWindow = 1500 * time.Millisecond

// idlePollInterval is the tick at which the idle gate re-checks the
// quiescence + foreground conditions.
const idlePollInterval = 150 * time.Millisecond

// exitWaitTimeout bounds how long we wait for the foreground command to
// leave the agent after `/exit` before falling back to SIGTERM.
// ForegroundComm is kernel truth but only ≤5s fresh (the sidecar's
// tcgetpgrp poller, fixed 5s), so this MUST comfortably exceed that
// staleness or a clean exit would race the cache and trip the kill
// fallback. 10s = 2× the poll window.
const exitWaitTimeout = 10 * time.Second

// preKillIdleTimeout bounds the SECOND idle re-confirmation done right
// before the SIGTERM fallback. If `/exit` failed to return the shell, we
// re-check quiescence: a still-quiet agent is wedged-at-prompt (safe to
// SIGTERM), but a noisy one is mid-tool with our `/exit` merely queued
// behind it — killing then would interrupt the tool (repo corruption),
// so we abort instead. Short: we only need to catch active output.
const preKillIdleTimeout = 4 * time.Second

// fgPollInterval is the tick at which the post-exit wait re-reads the
// foreground command.
const fgPollInterval = 200 * time.Millisecond

// killSettleWindow is the grace after a SIGTERM-fallback for the agent's
// process group to die and the shell to regain the foreground before we
// inject the restart command.
const killSettleWindow = 2 * time.Second

// preRestartSettle is a short pause after the shell returns (cleanly or
// via SIGTERM) before injecting the restart command, so the shell's
// line editor is ready to receive it.
const preRestartSettle = 400 * time.Millisecond

// restartCmd relaunches the agent reattaching to its prior conversation.
// `--resume` reattaches to the most recent session in the cwd's project
// so the in-memory conversation is restored from disk — the whole point
// of preferring this over a bare `claude` (the user keeps their work).
const restartCmd = "claude --resume\r"

// postRecoveryCooldown is how long the wedge watcher stays silenced
// after a successful save-restart. `claude --resume` repaints its
// scrollback by emitting many CUDs in rapid succession (history
// replay) — this matches the vertical_walk signature exactly and
// would otherwise re-pop the recovery banner the moment recovery
// finishes. 30s comfortably outlasts the longest restoration replay
// we've observed; after that the watcher returns to normal
// sensitivity and can catch a genuine fresh wedge.
const postRecoveryCooldown = 30 * time.Second

// waitForAgentIdle blocks until the session's PTY output has been quiet
// for at least quietWindow AND (when known) the foreground command is
// still `agent` — i.e. the agent is back at its input prompt with no
// tool running — or until timeout / ctx cancellation. Returns true only
// on a confirmed idle; false on timeout or cancellation (caller MUST
// then abort rather than force an exit, so a mid-tool agent is never
// interrupted).
//
// Quiescence is sampled via a PTY byte observer (fires from the Pump
// goroutine); the last-byte timestamp is mutex-guarded. The foreground
// check is a cheap cached read (≤5s fresh) used only as a secondary
// confirmation — quiescence is the primary, fast signal, so the fg
// staleness can't cause a premature "idle".
func waitForAgentIdle(
	ctx context.Context,
	sess *session.Session,
	agent string,
	quietWindow, timeout time.Duration,
) bool {
	var mu sync.Mutex
	last := time.Now()
	sess.SetPTYByteObserver(func(data []byte) {
		if len(data) == 0 {
			return
		}
		mu.Lock()
		last = time.Now()
		mu.Unlock()
	})
	defer sess.SetPTYByteObserver(nil)

	deadline := time.After(timeout)
	ticker := time.NewTicker(idlePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-ticker.C:
			mu.Lock()
			quietFor := time.Since(last)
			mu.Unlock()
			if quietFor < quietWindow {
				continue
			}
			// Output is quiet. If we know the foreground command,
			// require it to still be the agent: a tool that grabbed
			// the tty (fg != agent) means a turn is in flight even if
			// it happens to be momentarily silent.
			if agent != "" {
				if fg := sess.ForegroundComm(); fg != "" && fg != agent {
					continue
				}
			}
			return true
		}
	}
}

// waitForForegroundLeave blocks until the session's foreground command
// is no longer `agent` (the agent exited and the shell is back) or until
// timeout / ctx cancellation. Returns true once the agent has left,
// false otherwise. When the foreground command is unknown ("" agent —
// backend without the capability) it can't observe the transition, so it
// falls back to a fixed settle and reports success (assume exited).
func waitForForegroundLeave(
	ctx context.Context,
	sess *session.Session,
	agent string,
	timeout time.Duration,
) bool {
	if agent == "" {
		return sleepCtx(ctx, preRestartSettle)
	}
	deadline := time.After(timeout)
	ticker := time.NewTicker(fgPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-ticker.C:
			if fg := sess.ForegroundComm(); fg != agent {
				return true
			}
		}
	}
}

// runRecover drives the kill-and-resume restart of a session's
// foreground agent, preserving the conversation via `--resume`.
//
// Flow (v1.6.3 / B2 — idle-gated, save-prompt-free):
//  1. RecoverProgress {stage: "started"}.
//  2. IDLE GATE: wait for the agent's turn to complete (PTY output
//     quiescent + fg still the agent), bounded by grace. If it never
//     goes idle, ABORT with {stage: "error"} — we never interrupt a
//     running tool (repo-corruption safety). No save-prompt is injected;
//     `--resume` restores the conversation from disk.
//  3. Inject "/exit\r"; RecoverProgress {stage: "exiting"}.
//  4. Wait for the foreground to leave the agent (shell returned). If it
//     doesn't within exitWaitTimeout, SIGTERM the foreground group
//     (KillForeground) — safe because we already confirmed idle, and the
//     shell is in a different process group so it survives.
//  5. Inject "claude --resume\r"; RecoverProgress {stage: "restarting"}.
//  6. ResetWedge() + post-recovery cooldown for the replay storm.
//  7. RecoverProgress {stage: "done"}.
//
// Errors at any injection point emit {stage: "error"} and return. The
// whole sequence honours ctx cancellation between stages so a client
// disconnect terminates the goroutine promptly.
//
// The sequencer does NOT hold the session lock or block the read pump —
// each WriteStdin goes through Session.WriteStdin (own locking).
func runRecover(
	ctx context.Context,
	sess *session.Session,
	req protocol.Recover,
	write frameWriter,
) {
	grace := time.Duration(req.GraceMillis) * time.Millisecond
	if grace <= 0 {
		grace = defaultGrace
	}
	if grace > maxGrace {
		grace = maxGrace
	}

	sid := sess.ID().String()
	emit := func(stage, detail string) {
		body, err := protocol.MarshalRecoverProgress(protocol.RecoverProgress{
			Stage:  stage,
			Detail: detail,
		})
		if err != nil {
			slog.Warn("recover: marshal RecoverProgress failed",
				"sid", sid, "stage", stage, "err", err)
			return
		}
		if werr := write(protocol.FrameTypeControl, body); werr != nil {
			slog.Warn("recover: write RecoverProgress failed",
				"sid", sid, "stage", stage, "err", werr)
		}
	}

	// The foreground command at sequence start — used to confirm idle
	// (fg == agent) and to detect exit (fg leaves agent). "" when the
	// backend can't report it; the helpers degrade gracefully.
	agent := sess.ForegroundComm()

	slog.Info("recover: sequence started",
		"sid", sid, "grace_ms", grace/time.Millisecond, "agent", agent)
	emit(protocol.RecoverStageStarted, "")

	// 1. Idle gate. Never proceed while a tool is running — interrupting a
	//    Bash tool mid-flight (git commit, package install) is the repo-
	//    corruption surface. Abort (don't force) if the agent stays busy.
	emit(protocol.RecoverStageSaving, "Waiting for the agent to finish…")
	if !waitForAgentIdle(ctx, sess, agent, idleQuietWindow, grace) {
		if ctx.Err() != nil {
			emit(protocol.RecoverStageError, "cancelled while waiting for idle")
		} else {
			emit(protocol.RecoverStageError, "agent still busy — try again when it's idle")
			slog.Info("recover: aborted — agent never went idle", "sid", sid, "grace", grace)
		}
		return
	}

	// 2. Clean exit.
	emit(protocol.RecoverStageExiting, "")
	if err := injectAndCheckCtx(ctx, sess, []byte("/exit\r")); err != nil {
		emit(protocol.RecoverStageError, "exit command failed: "+err.Error())
		return
	}

	// 3. Wait for the shell to return; SIGTERM fallback if it doesn't.
	if !waitForForegroundLeave(ctx, sess, agent, exitWaitTimeout) {
		if ctx.Err() != nil {
			emit(protocol.RecoverStageError, "cancelled while awaiting exit")
			return
		}
		// /exit didn't return the shell. Before escalating to SIGTERM,
		// RE-CONFIRM the agent is idle: the initial idle gate could have
		// caught a lull, and if output is flowing now a tool is running
		// with our `/exit` queued behind it — a SIGTERM here would kill
		// that tool mid-flight (repo corruption). Only a still-quiet,
		// not-exiting agent is wedged-at-prompt and safe to terminate; a
		// noisy one means abort and let the user retry when it's idle.
		if !waitForAgentIdle(ctx, sess, agent, idleQuietWindow, preKillIdleTimeout) {
			if ctx.Err() != nil {
				emit(protocol.RecoverStageError, "cancelled awaiting exit")
			} else {
				emit(protocol.RecoverStageError, "agent busy after /exit — not forcing a kill")
				slog.Info("recover: aborted SIGTERM — agent producing output after /exit", "sid", sid)
			}
			return
		}
		// Confirmed idle but still foreground: input pipeline is wedged.
		// SIGTERMing the foreground group is safe — no tool is running,
		// and the shell (a different process group) survives.
		slog.Info("recover: /exit did not return shell, agent idle — SIGTERM fg fallback", "sid", sid)
		if err := sess.KillForeground(); err != nil {
			slog.Warn("recover: KillForeground failed", "sid", sid, "err", err)
		}
		if !sleepCtx(ctx, killSettleWindow) {
			emit(protocol.RecoverStageError, "cancelled after fg kill")
			return
		}
	}

	if !sleepCtx(ctx, preRestartSettle) {
		emit(protocol.RecoverStageError, "cancelled before restart")
		return
	}

	// 4. Restart with --resume (restores the conversation from disk). If
	//    --resume fails (no prior session for this cwd, not in PATH) the
	//    user sees the shell error and can rerun — best-effort.
	emit(protocol.RecoverStageRestarting, "")
	if err := injectAndCheckCtx(ctx, sess, []byte(restartCmd)); err != nil {
		emit(protocol.RecoverStageError, "restart command failed: "+err.Error())
		return
	}

	// 5. Reset the wedge watcher: the fresh `--resume` Claude owns a
	//    brand-new Ink renderer with zero accumulated drift, so the
	//    lifetime resize/byte counters and any in-flight resize-scan
	//    window must reset too. Without this the pre-restart accumulation
	//    survives and the next keyboard resize re-trips the detector on
	//    what is now a healthy session.
	sess.ResetWedge()

	// 6. Post-recovery cooldown for the --resume scrollback replay (many
	//    CUDs in rapid succession that otherwise re-pop the banner).
	sess.SuppressWedgeUntil(time.Now().Add(postRecoveryCooldown))

	emit(protocol.RecoverStageDone, "")
	slog.Info("recover: sequence completed",
		"sid", sid,
		"cooldown_until", time.Now().Add(postRecoveryCooldown).Format(time.RFC3339))
}

// injectAndCheckCtx writes data to the session's PTY stdin and
// returns the ctx error if cancelled mid-write. Wraps WriteStdin's
// error to keep callers' error paths uniform.
func injectAndCheckCtx(ctx context.Context, sess *session.Session, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := sess.WriteStdin(data)
	return err
}

// sleepCtx sleeps for d unless ctx cancels first. Returns true on
// normal completion, false on cancellation — callers use this to
// short-circuit the sequencer when the client disconnects.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
