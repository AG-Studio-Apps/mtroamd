package transport

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/altscreen"
	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// drainBriefly runs an unbuffered drain on the control stream for
// up to `d` so the AttachAck failure frame we just wrote actually
// hits the wire before the deferred CloseWithError tears the
// connection down. quic-go's Stream.Close marks the write side
// done but doesn't block until bytes drain — without the drain
// the client sees a bare CONNECTION_CLOSE instead of our typed
// AttachAck error message. Behaviour is identical on plain TCP:
// the deadline + Discard idiom drains either transport.
//
// Audit F-G (v0.0.2 review): replaces an earlier ctxReader pattern
// that spawned a child goroutine per Read and leaked it until
// teardown. SetReadDeadline does the same job natively without
// the goroutine cost.
func drainBriefly(s Conn, d time.Duration) {
	_ = s.SetReadDeadline(time.Now().Add(d))
	_, _ = io.Copy(io.Discard, s)
}

// ProtocolHandler is the real Handler that drives the mtRoam protocol
// per docs/mtroam-protocol.md. One ProtocolHandler is shared across
// all accepted connections — it holds no per-connection state, only
// the session.Registry it dispatches into.
//
// HandleConnection orchestrates the per-attach goroutines and waits
// for any of them to return before tearing down. The whole protocol
// runs on one client-initiated bidirectional stream: every frame is
// wrapped in a `[u8 type][u32 len][body]` tagged envelope, where
// type ∈ {control, stdin, stdout}. The Attach handshake is the
// first frame; AttachAck is the response.
//
// We use a single stream rather than QUIC multistream because iOS
// Network.framework's NWMultiplexGroup-based multistream API has
// proved unworkable: NWConnection(from: NWConnectionGroup) returns
// nil if called immediately after group.start, and the parent group
// never transitions to .ready without a child being opened first
// (chicken-and-egg). Type-tagged framing on a single bidi stream
// keeps the persistence + replay-on-reattach semantics intact
// without depending on Apple's multistream API.
type ProtocolHandler struct {
	Registry *session.Registry
	Logger   *slog.Logger

	// Limiter records attach-token validation failures so a source that
	// repeatedly presents bad tokens is put into an accept cooldown by
	// the transports. Nil disables it. Shared with the QUIC + TCP servers.
	Limiter *SourceLimiter

	// PTYSpawner is the lazy-spawn factory the handler calls when a
	// client attaches to a session that was restored from disk by
	// LoadPersisted (and therefore has Session.pty == nil). The daemon
	// wires this to a closure that spawns either an in-process
	// pty.Handle or a sidecar-backed ptyclient.Conn (per-session,
	// crash-survivable), then starts Session.Pump in a new goroutine.
	// The *Session is passed in so sidecar-backed spawns can name
	// their per-session state dir by the SessionID. Nil-safe: when
	// this field is nil, restored sessions can't be reattached (the
	// AttachAck reports a generic error) — tests that don't exercise
	// restore can leave it unset.
	PTYSpawner func(sess *session.Session, rows, cols uint16) (session.PTY, error)
}

// HandleConnection implements Handler. The Conn argument bundles the
// single bidirectional byte stream (QUIC accepted stream or raw TCP
// conn) with connection-level operations the protocol needs. The
// QUIC AcceptStream / TCP no-op-because-already-a-stream step
// happens in the respective Server's accept loop before the call,
// so this function is transport-agnostic.
func (h *ProtocolHandler) HandleConnection(ctx context.Context, ctrl Conn) {
	log := h.logger().With("remote", ctrl.RemoteAddr().String())
	log.InfoContext(ctx, "accepted connection")

	// Default close: 0 + empty (graceful). Pumps may overwrite if
	// they hit a protocol violation. On QUIC this becomes the
	// CONNECTION_CLOSE typed code; on TCP it's discarded — the
	// mtRoam framing layer already wrote a typed Goodbye / AttachAck
	// frame on the wire before we land here.
	closeErr := uint64(0)
	closeMsg := ""
	defer func() {
		_ = ctrl.CloseWithError(closeErr, closeMsg)
	}()

	att, err := readAttach(ctrl)
	if err != nil {
		log.WarnContext(ctx, "read Attach", "err", err)
		// closeMsg goes on the wire in CONNECTION_CLOSE; never
		// echo peer-supplied bytes back to the peer (audit F8 —
		// peer can shape err.Error() via the "got %q" formatter).
		// Use a small fixed table keyed on err class instead.
		closeErr = errCodeFor(err)
		closeMsg = closeMsgFor(closeErr)
		return
	}
	// Pre-auth read deadline (set by TCPServer.Serve) has done its
	// job — Attach arrived in time. Clear it so the live session
	// isn't subject to a 5-second idle reset. QUIC connections never
	// had a deadline set; for them this is a harmless no-op (a zero
	// time.Time clears the deadline on net.Conn, and *quic.Stream
	// treats it the same).
	_ = ctrl.SetReadDeadline(time.Time{})

	sess, err := h.resolveAttach(att, ctrl, ipFromAddr(ctrl.RemoteAddr()))
	if err != nil {
		// resolveAttach already wrote the AttachAck failure response.
		// Close the control stream's write side, then drain briefly
		// so the AttachAck makes it on the wire before the deferred
		// CloseWithError tears the connection down.
		_ = ctrl.Close()
		drainBriefly(ctrl, 500*time.Millisecond)
		log.InfoContext(ctx, "attach rejected", "err", err)
		return
	}

	// Resolve the requested attach mode. Empty / unknown → exclusive
	// (back-compat with v0 clients that don't set the field).
	attachMode := session.AttachExclusive
	switch att.Mode {
	case protocol.AttachModeReadonly:
		attachMode = session.AttachReadonly
	case protocol.AttachModePassive:
		attachMode = session.AttachPassive
	case protocol.AttachModeExclusiveIfFree:
		// Resolved to exclusive-or-readonly inside Acquire, under
		// the session lock; attachMode is reassigned to the granted
		// mode below so every downstream gate (stdin, resize,
		// recover, wedge push) follows the granted role.
		attachMode = session.AttachExclusiveIfFree
	}

	// Lazy-spawn the PTY for sessions hydrated by LoadPersisted. The
	// session's scrollback is already populated from the on-disk
	// snapshot; we just need a fresh shell process to attach to.
	// Capture the restored flag NOW so the AttachAck a few lines
	// below can report it (AssignPTY clears the flag).
	wasRestored := sess.RestoredFromDisk()
	if wasRestored {
		if err := h.lazySpawnRestoredPTY(ctx, sess, att.Rows, att.Cols, log); err != nil {
			_ = sendAttachAck(ctrl, protocol.AttachAck{
				V:   1,
				Err: protocol.AttachErrUnknownSession,
				Msg: "restored session: " + err.Error(),
			})
			return
		}
	}

	// Acquire the session — exclusive displaces any prior exclusive
	// (whose pumps observe attachCtx.Done() and unwind), readonly
	// just adds to the live-clients slice. Either way, gen is what
	// we pass to Release on exit so a displaced re-entry doesn't
	// clobber the new owner (audit F4).
	attachCtx, attachGen, grantedMode, err := sess.AcquireClient(ctx, attachMode, att.ClientID)
	// From here on attachMode is the GRANTED role, which differs from
	// the request only for exclusive-if-free (→ exclusive or readonly).
	attachMode = grantedMode
	if err != nil {
		ackErr := protocol.AttachErrUnknownSession
		if errors.Is(err, session.ErrPassiveCapacity) ||
			errors.Is(err, session.ErrReadonlyCapacity) {
			ackErr = protocol.AttachErrCapacity
		}
		_ = sendAttachAck(ctrl, protocol.AttachAck{
			V:   1,
			Err: ackErr,
			Msg: err.Error(),
		})
		return
	}
	defer sess.Release(attachGen)

	// Only the exclusive client owns the PTY size. Readonly clients'
	// Rows/Cols on the Attach are the dimensions of THEIR local
	// terminal — they observe whatever the exclusive client is
	// driving, even if mismatched. Honouring readonly resize would
	// fight the exclusive client's geometry and cause SIGWINCH
	// thrashing.
	//
	// Pre-v1.0 hardening (audit Finding 7.1): mirror the F-I bounds
	// check from handleControlFrame's Resize path so the two entry
	// points to sess.Resize are symmetric. An exclusive client could
	// previously supply Rows=1 or Rows=65535 on Attach and bypass the
	// clamp; out-of-range dims are now silently ignored (the prior
	// >0 gate kept zero-dim Attaches from resizing, but accepted
	// everything above zero).
	// Resize the PTY + live screen-model to the exclusive client's geometry, and
	// latch whether THIS attach changed the grid size. `attachResizedGrid` is an
	// ATOMIC local decision (pre-resize geometry vs the attach dims) — unlike the
	// model's resize-dirty flag it CANNOT be raced by the Pump goroutine clearing
	// dirty between the resize and the inject below (the Pump feeds the model
	// under screenMu in a separate acquisition). A geometry change top-anchors
	// the model (it can't know where the app will redraw — a grow strands a
	// bottom-anchored prompt mid-screen, a shrink drops it) and SIGWINCHes the
	// app, so we skip BOTH the injected redraw and the footer re-emit and let raw
	// replay carry the app's fresh repaint (rc5 behaviour, known-good for
	// cold-start). A same-geometry attach is a no-op resize; the model's own
	// resize-dirty flag (checked in InjectAltScreenRepaint + the footer gate)
	// then catches a still-un-repainted MID-SESSION resize, and otherwise the
	// prime fires for its value case (footer aged out, no fresh repaint coming).
	prevRows, prevCols := sess.WindowSize()
	attachResizedGrid := attachMode == session.AttachExclusive &&
		dimsInBounds(att.Rows, att.Cols) &&
		(prevRows != att.Rows || prevCols != att.Cols)
	if attachMode == session.AttachExclusive && dimsInBounds(att.Rows, att.Cols) {
		_ = sess.Resize(att.Rows, att.Cols)
	}

	buf := sess.Buffer()
	if buf == nil {
		_ = sendAttachAck(ctrl, protocol.AttachAck{
			V:   1,
			Err: protocol.AttachErrUnknownSession,
			Msg: "session closed",
		})
		return
	}

	// Alt-screen state only matters when a budget came (the clamp is
	// budget-gated) — skip the two mutex hops for budget-less CLI
	// attaches.
	altActive := att.ReplayBudget > 0 && sess.WedgeAltScreenActive()

	// Full-frame redraw (preferred): when a full-screen TUI is running and
	// the live screen-model is faithful, inject the WHOLE synthesized clean
	// frame and pin the replay window to start right before it (below), so
	// the client replays ONLY the redraw — not the truncated raw byte tail
	// it cannot reassemble into a 2-D screen. This is the fix for cold-start
	// alt-screen spill: the daemon hands the client a complete screen, the
	// way tmux/mosh do. fullRedrawStart marks the pre-inject head; the
	// override after computeReplayWindow clamps `start` to it.
	// Gate the redraw on the client having a replay budget (budget-less CLI
	// attaches keep full raw replay) and on the live screen-model's OWN
	// authoritative alt+faithful check inside InjectAltScreenRepaint — NOT on
	// the wedge heuristic (altActive), so a wedge miss can't suppress a redraw
	// the model can serve.
	fullRedraw := false
	var fullRedrawStart uint64
	// Read once and reuse for the footer-re-emit gate below (single lock hop; no
	// TOCTOU between two separate reads). dirty can only go true→false during an
	// attach (Pump feeding the app's repaint; no concurrent Resize mid-handshake),
	// so a captured value is never falsely stale in the unsafe direction.
	var modelResizeDirty bool
	if att.ReplayBudget > 0 {
		// One screenMu hop for all four fields, so the logged state is internally
		// consistent (a Pump Feed can't land between separate reads).
		hasScr, altScr, faithScr, dirtyScr := sess.ScreenSnapshot()
		modelResizeDirty = dirtyScr
		// Skip the inject when THIS attach resized (the atomic latch above): the
		// model is top-anchored and the app will SIGWINCH-repaint, so raw replay
		// carries it. On a same-geometry attach, InjectAltScreenRepaint is
		// authoritative — it still refuses (ok=false) on a main-buffer, unfaithful,
		// or resize-dirty model (the last catches a MID-SESSION resize whose
		// repaint hasn't landed), so a stale/top-anchored grid is never shipped.
		if !attachResizedGrid {
			if start, ok := sess.InjectAltScreenRepaint(); ok {
				fullRedraw = true
				fullRedrawStart = start
			}
		}
		slog.Info("attach.altredraw",
			"sid", sess.ID().String(),
			"budget", att.ReplayBudget,
			"wedgeAlt", altActive,
			"modelHas", hasScr,
			"modelAlt", altScr,
			"modelFaithful", faithScr,
			"attachResized", attachResizedGrid,
			"modelResizeDirty", modelResizeDirty,
			"injected", fullRedraw,
			"start", fullRedrawStart)
	}

	// Footer re-emit (fallback): only when the full-model redraw didn't
	// fire (model unfaithful / stale / not on the alt buffer). The status
	// footer + the prompt's border block are event-driven (drawn once, not
	// on every output frame like the spinner / conversation), so they age
	// out of the recency-based replay window and a reattaching client
	// rebuilds the screen WITHOUT them. Reconstruct the bottom rows from the
	// ring and INJECT them as real bytes BEFORE computing the window — so
	// they're the newest content and ride the NORMAL replay (real seqs, no
	// client/protocol change). Gated on the reconstruct staying FAITHFUL:
	// vim/htop (scroll region / IL / DL) get nothing — they have no such
	// footer. The redraw is cursor-neutral (ESC7/ESC8), so other attached
	// clients see only an idempotent in-place refresh of the bottom rows.
	//
	// Also skip on a resized attach (the atomic latch OR a still-dirty model):
	// like the full redraw, ReconstructBottomRows would place the bottom rows at
	// the NEW geometry from OLD-geometry ring bytes (a grow misplaces them — the
	// same regression the full-redraw gate guards). The app's SIGWINCH repaint
	// carries the footer on a resized attach instead.
	if altActive && !fullRedraw && !attachResizedGrid && !modelResizeDirty {
		tail := buf.TailSeq()
		if ring, _, _ := buf.ReadSince(tail, int(buf.HeadSeq()-tail)); len(ring) > 0 {
			rows, cols := sess.WindowSize()
			if redraw, faithful := altscreen.ReconstructBottomRows(
				ring, int(rows), int(cols), altScreenFooterRows); faithful && len(redraw) > 0 {
				_, _ = sess.InjectOutput(redraw)
			}
		}
	}

	// Sanitize stranded MOUSE mode on attach. A TUI/agent killed ungracefully
	// (SIGKILL, host reboot) never emits its mouse-off DECRSTs, so the replayed
	// ring carries mouse-ON but not the matching OFF, and a reattaching client
	// comes up with the scroll wheel hijacked by mouse reporting. Inject the
	// mouse-off DECRSTs as the NEWEST ring bytes so they ride the tail of the
	// replay and cancel any replayed mouse-ON. Scoped to mouse only (NOT
	// bracketed paste ?2004, which the shell's own readline enables and wants
	// kept). Reset when EITHER:
	//   - this attach just lazy-spawned a restored session (`wasRestored`): the
	//     freshly spawned shell can't want mouse, and its fg poller hasn't
	//     sampled yet (comm reads "" for the first ~5s) — this is the reboot
	//     case, where the stranded mouse-ON was just replayed from disk; or
	//   - the live foreground is a plain shell (the TUI exited on a still-up box).
	// Gated so a live mouse-using TUI is never disabled out from under it.
	if att.ReplayBudget > 0 {
		comm, _, _, _ := sess.ForegroundSnapshot()
		if shouldResetStrandedMouse(att.ReplayBudget, wasRestored, comm) {
			_, _ = sess.InjectOutput([]byte("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l"))
		}
	}

	start, head, trunc := computeReplayWindow(
		buf, att.AckSeq, att.ReplayBudget, altActive)

	// When we injected a full-model redraw, replay ONLY it (plus any live
	// PTY bytes that landed after): the synthesized frame IS the complete
	// screen, so everything before it is redundant and the raw tail is
	// unreassemblable. A frame is ATOMIC — a small ReplayBudget must never
	// slice `start` into the middle of it (computeReplayWindow would pin
	// start = head - capBytes, landing inside the frame and replaying a
	// half-frame: garbage, worse than the bug we're fixing). So pin `start`
	// to the pre-inject head whenever we injected, even when that EXTENDS
	// the window past the budget cap: one frame is bounded (< a worst-case
	// styled screen ≪ AltScreenReplayCap) and coherent. trunc stays true
	// (pre-redraw output was intentionally skipped) — matching the existing
	// alt-screen contract the client already handles (no scrollback to lose).
	if fullRedraw && fullRedrawStart <= head {
		start = fullRedrawStart
		trunc = true
	}

	// Sync writes on the single stream — outputPump and the read
	// pump's control responses (Pong, AttachAck etc.) both call
	// writeFrame; quic-go's Stream.Write is not safe for concurrent
	// callers, so we serialise via this mutex.
	var writeMu sync.Mutex
	writeFrame := func(t uint8, body []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return protocol.WriteTaggedFrame(ctrl, t, body)
	}

	// Wedge push: the wedge watcher fires a callback on every
	// detection; we translate to a protocol.WedgeDetected frame and
	// send it on the existing control stream. Installed only for
	// exclusive attaches — readonly / passive clients shouldn't
	// trigger recovery UI in the user's app, and only the exclusive
	// client owns the resize path that produces wedge candidates in
	// the first place. Cleared on attach exit (deferred below) so a
	// re-attach gets a fresh closure pointed at the new stream.
	// Displacement notice (first-attach-wins, v1.7.0): when a later
	// exclusive attach displaces this client, the session fires this
	// notifier BEFORE cancelling our context, while the stream is
	// still healthy. We push Goodbye{reason:"replaced"} here so the
	// client can tell displacement from a network drop (and e.g.
	// rejoin readonly instead of redialing exclusive — the
	// two-device attach-war fix), and flag the teardown as graceful
	// so the output pump's ctx-cancel hook FINs the stream (delivering
	// the queued Goodbye) instead of resetting it. The gen-keyed
	// notifier dies with this attach (Release drops the entry), so no
	// defer-clear is needed. Readonly/passive clients are never
	// displaced — exclusive-only by construction. writeFrame is
	// mutex-serialised, so writing from the displacer's goroutine is
	// safe against the pumps.
	var wasDisplaced atomic.Bool
	if attachMode == session.AttachExclusive {
		sess.SetReplacedNotifier(attachGen, func() {
			wasDisplaced.Store(true)
			body, err := protocol.MarshalGoodbye(protocol.Goodbye{
				Reason: protocol.ReasonReplaced,
			})
			if err != nil {
				return
			}
			if werr := writeFrame(protocol.FrameTypeControl, body); werr != nil {
				log.DebugContext(ctx, "replaced: Goodbye push failed (stream tearing down?)",
					"err", werr)
			} else {
				log.InfoContext(ctx, "replaced: pushed Goodbye to displaced client",
					"session", sess.ID().String(), "gen", attachGen)
			}
		})

		sess.OnWedge(func(n session.WedgeNotice) {
			body, err := protocol.MarshalWedgeDetected(protocol.WedgeDetected{
				Kind:               n.Kind,
				SessionAgeSec:      n.SessionAgeSec,
				TotalOutBytes:      n.TotalOutBytes,
				OldRows:            n.OldRows,
				NewRows:            n.NewRows,
				ResizesObserved:    n.ResizesObserved,
				SilentWedges:       n.SilentWedges,
				CursorWedges:       n.CursorWedges,
				VerticalWalkWedges: n.VerticalWalkWedges,
				CudObserved:        n.CudObserved,
				CursorRowSeen:      n.CursorRowSeen,
			})
			if err != nil {
				slog.Warn("wedge: marshal WedgeDetected failed",
					"sid", sess.ID().String(), "err", err)
				return
			}
			// The wedge JSONL on disk is the durable record; the client
			// push is a UX hint. Log the push so a transient wedge can be
			// traced end-to-end against the iOS [wedge] logs (telling us
			// which hop it died at). Write errors are expected during
			// stream teardown.
			if werr := writeFrame(protocol.FrameTypeControl, body); werr != nil {
				slog.Debug("wedge: push to client failed (stream tearing down?)",
					"sid", sess.ID().String(), "err", werr)
			} else {
				slog.Info("wedge: pushed WedgeDetected to client",
					"sid", sess.ID().String(), "kind", n.Kind,
					"old_rows", n.OldRows, "new_rows", n.NewRows, "cud", n.CudObserved)
			}
		})
		defer sess.OnWedge(nil)
	}

	resolvedMode := protocol.AttachModeExclusive
	switch attachMode {
	case session.AttachReadonly:
		resolvedMode = protocol.AttachModeReadonly
	case session.AttachPassive:
		resolvedMode = protocol.AttachModePassive
	}
	// Atomically read+clear the first-attach flag so exactly one
	// AttachAck per session carries FreshlyCreated=true. Restored
	// sessions arrive with the flag already false (LoadPersisted
	// path) so a restore reports Restored=true, FreshlyCreated=false.
	freshlyCreated := sess.ConsumeFirstAttach()
	// One CONSISTENT fg snapshot for the AttachAck (critic #1/#2): refreshes the
	// anchor so an idle fg transition is reflected, and reads Fg/FgSince/FgSinceSeq/
	// Cwd together so they can't tear. attachFg is also the attach log + the ticker's
	// change-detection seed.
	attachFg, fgSince, fgSinceSeq, fgCwd := sess.ForegroundSnapshot()
	ackBody, err := protocol.MarshalAttachAck(protocol.AttachAck{
		V:               1,
		OK:              true,
		SessionID:       sess.ID().Bytes(),
		Start:           start,
		BufSeq:          head,
		Trunc:           trunc,
		Mode:            resolvedMode,
		Peers:           sess.PeerModes(attachGen),
		Restored:        wasRestored,
		FreshlyCreated:  freshlyCreated,
		RTTNanos:        rttNanosFor(ctrl),
		AltScreenActive: sess.WedgeAltScreenActive(),
		LastTitle:       sess.LastTitle(),
		// fg transition anchors (v1.6.3+): time + ring byte-seq of the
		// last foreground change, plus its cwd. See fgSinceToNanos.
		Fg:         attachFg,
		FgSince:    fgSinceToNanos(fgSince),
		FgSinceSeq: fgSinceSeq,
		Cwd:        fgCwd,
		Backend:    backendFor(sess),
	})
	if err != nil {
		log.WarnContext(ctx, "marshal AttachAck", "err", err)
		return
	}
	if err := writeFrame(protocol.FrameTypeControl, ackBody); err != nil {
		log.WarnContext(ctx, "send AttachAck", "err", err)
		return
	}

	// ★★ REATTACH REPAINT NUDGE — provoke the app to redraw itself, AFTER the ack.
	//
	// A same-geometry reattach signals nobody: SetSize with unchanged dimensions changes
	// nothing, so no SIGWINCH is raised and the app never repaints. The client is handed a
	// frame reconstructed from the daemon's model, and the whole trust apparatus above
	// exists to judge that reconstruction. Rotating the phone changes the geometry, the app
	// redraws, and the screen is correct — that has always been the user's workaround.
	//
	// So do it deliberately: drop a row, hold, restore. The app's repaint then arrives as
	// ordinary LIVE output on top of whatever the attach already delivered.
	//
	// ★ It is deliberately ADDITIVE. An earlier revision skipped the synthesised frame and
	// tried to prove the app had repainted before doing so; there is no app-agnostic way to
	// prove that, because on a streaming session any output looks like a repaint, and the
	// false positive threw away the only complete screen the daemon can produce. The frame
	// is now always the floor: worst case the client gets exactly what it gets today, best
	// case the app's own repaint corrects it a moment later.
	//
	// Placed after the ack so it adds nothing to attach latency and cannot poison the frame
	// that has already been chosen.
	if !attachResizedGrid && altActive && attachMode == session.AttachExclusive {
		// ★ Require the MODEL to agree that a full-screen app is running, not just the
		// wedge watcher. The watcher's flag is seeded from the DEAD app's persisted meta
		// on the restore path and lazySpawnRestoredPTY does not clear it, so on its own it
		// will happily authorise a PTY mutation against a freshly spawned bash prompt —
		// which is the main-buffer double-redraw this is gated to avoid.
		if _, modelAlt, _, _ := sess.ScreenSnapshot(); modelAlt &&
			len(sess.PeerModes(attachGen)) == 0 {
			sess.NudgeRepaint(nudgeHoldWindow)
		}
	}

	// Structured attach event — operators tailing the daemon's
	// stderr can see who's coming and going, and a multi-attach
	// debugging session can pivot on session_id + mode + peer
	// counts without grepping through free-text log lines.
	log.InfoContext(ctx, "session.attach",
		"session", sess.ID().String(),
		"name_hash", session.NameHash(sess.ID(), sess.Name()),
		"mode", resolvedMode,
		"gen", attachGen,
		"peers", len(sess.PeerModes(attachGen)),
		"replay_start", start,
		"replay_truncated", trunc,
		"replay_budget", att.ReplayBudget,
		"replay_bytes", head-start,
		"fg", attachFg,
	)
	log.DebugContext(ctx, "session.attach.name",
		"session", sess.ID().String(),
		"name", sess.Name(),
	)
	defer log.InfoContext(ctx, "session.detach",
		"session", sess.ID().String(),
		"name_hash", session.NameHash(sess.ID(), sess.Name()),
		"mode", resolvedMode,
		"gen", attachGen,
	)

	pumpsCtx, pumpsCancel := context.WithCancel(attachCtx)
	defer pumpsCancel()

	// recoverPump contains a panic to a single per-attach goroutine.
	// Each pump runs on its own goroutine, so the connection-level
	// recover in the accept loop can't catch them — an unrecovered
	// panic in any pump would crash the whole daemon and every other
	// live session. On panic: log, cancel the pump group so the
	// siblings exit and the connection tears down cleanly, then let
	// the deferred wg.Done fire.
	recoverPump := func(name string) {
		if r := recover(); r != nil {
			log.ErrorContext(ctx, "transport: recovered panic in pump",
				"pump", name, "session", sess.ID().String(), "panic", r)
			pumpsCancel()
		}
	}

	var wg sync.WaitGroup
	wg.Add(4)

	// RTT notify: every 5 seconds while attached, emit an RTTNotify
	// control frame with quic-go's smoothed-RTT estimate. Clients drive
	// `--predict=adaptive` thresholds + `~?` info display from this.
	// One ticker per attach — RTT can drift independently per path,
	// and the per-client cost is one syscall + one frame every 5s.
	go func() {
		defer wg.Done()
		defer recoverPump("rtt+fg")
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		// Foreground-command change detection (v1.6.1+) rides the
		// same tick. Seeded from the AttachAck's value so the first
		// AgentNotify only fires on a genuine post-attach change.
		// Unlike RTTNotify this is NOT rtt-gated — TCP/tsnet clients
		// (which never receive RTTNotify) still get fg updates.
		lastFg := attachFg
		for {
			select {
			case <-pumpsCtx.Done():
				return
			case <-ticker.C:
				if fg, fgSince, fgSinceSeq, fgCwd := sess.ForegroundSnapshot(); fg != lastFg {
					lastFg = fg
					// Carry the same fg transition anchors + cwd as AttachAck,
					// from ONE consistent snapshot (critic #1/#2), so a client
					// attached before this change gets a fresh, non-torn age/size
					// clock and cwd.
					notify := protocol.AgentNotify{
						Fg:         fg,
						FgSince:    fgSinceToNanos(fgSince),
						FgSinceSeq: fgSinceSeq,
						Cwd:        fgCwd,
					}
					if body, err := protocol.MarshalAgentNotify(notify); err == nil {
						// Best-effort, same posture as RTTNotify.
						_ = writeFrame(protocol.FrameTypeControl, body)
					}
				}
				rtt := rttNanosFor(ctrl)
				if rtt <= 0 {
					continue // handshake hasn't seeded the smoothing window
					// yet, or transport (TCP) doesn't measure RTT.
				}
				body, err := protocol.MarshalRTTNotify(protocol.RTTNotify{RTTNanos: rtt})
				if err != nil {
					continue
				}
				// Best-effort: write errors mean the connection is dying;
				// other pumps will observe ctx.Done() shortly.
				_ = writeFrame(protocol.FrameTypeControl, body)
			}
		}
	}()

	// Output pump: a PTY session streams its ring buffer as FrameTypeStdout;
	// a stream-backed session forwards its opaque published frames as control
	// frames (see streamOutputPump). Selection is server-side per session type.
	go func() {
		defer wg.Done()
		defer pumpsCancel()
		defer recoverPump("output")
		var err error
		if sess.IsStreamBacked() {
			err = streamOutputPump(pumpsCtx, sess, ctrl, writeFrame, &wasDisplaced)
		} else {
			err = outputPump(pumpsCtx, sess, ctrl, writeFrame, start, &wasDisplaced)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.DebugContext(pumpsCtx, "output pump exit", "err", err)
		}
	}()

	// Read pump: parse tagged frames from the single stream and
	// dispatch by type. Control frames (Ack/Resize/Ping/Goodbye)
	// route through the existing control handler; stdin frames
	// stream into the PTY.
	go func() {
		defer wg.Done()
		defer pumpsCancel()
		defer recoverPump("read")
		if err := readPump(pumpsCtx, sess, ctrl, writeFrame, attachMode, attachGen); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			log.DebugContext(pumpsCtx, "read pump exit", "err", err)
		}
	}()

	// Echo watcher (Stage B): polls the PTY's slave-side ECHO termios
	// flag and emits EchoConfirm frames whenever it flips. Clients use
	// these to authoritatively toggle predictive-echo arming (vim
	// entry disarms instantly; shell prompt return rearms). One
	// watcher per attach for simplicity — duplicate syscalls across
	// co-attached clients are cheap. No-op if the PTY implementation
	// doesn't support tcgetattr (e.g., the pipe-backed test PTY).
	go func() {
		defer wg.Done()
		defer recoverPump("echo")
		sess.WatchTermios(pumpsCtx, session.DefaultEchoPollInterval, func(s session.TermiosSnapshot) {
			body, err := protocol.MarshalEchoConfirm(protocol.EchoConfirm{
				EchoState: string(s.Echo),
				CanonMode: s.Canon == session.EchoStateOn,
			})
			if err != nil {
				return
			}
			// Write errors are silent — if the stream is dead, the
			// read/output pumps will exit on their own and tear the
			// connection down. EchoConfirm is best-effort by spec.
			_ = writeFrame(protocol.FrameTypeControl, body)
		})
	}()

	wg.Wait()
	// Displaced teardown: the Goodbye{replaced} was pushed by the
	// notifier (pre-cancel) and the output pump FIN'd the stream
	// instead of resetting it. Linger briefly so the peer can read
	// the frame before the deferred CloseWithError discards
	// undelivered stream data. A drain (attach-reject pattern) won't
	// work here: readPump's cancel hook has already CancelRead the
	// stream, so reads return instantly. Bounded, displacement-only.
	if wasDisplaced.Load() {
		time.Sleep(displacedCloseLinger)
	}
	log.InfoContext(ctx, "connection closed", "session", sess.ID().String())
}

// displacedCloseLinger is how long a displaced client's handler holds
// the connection open after its pumps exit so the Goodbye{replaced}
// (already FIN-queued on the stream) is delivered before the
// connection-level close discards it. Loopback delivers in <1ms; one
// lossy-WAN retransmit fits comfortably. Worst case the client misses
// the frame and degrades to the legacy bare-close behaviour.
const displacedCloseLinger = 250 * time.Millisecond

// lazySpawnRestoredPTY handles the first-attach-after-restart path
// for a session that was hydrated from disk by LoadPersisted. The
// session's scrollback is already populated from the snapshot, but
// the PTY field is nil (no shell process — the previous daemon
// instance's child died on its SIGTERM). We spawn a fresh shell at
// the session's saved dimensions, hand it to Session.AssignPTY, and
// start the Pump goroutine so subsequent output flows into the same
// ring buffer the restored scrollback lives in.
//
// Concurrency: two simultaneous attaches can race here. AssignPTY's
// nil-check + ErrSessionHasPTY tells the loser they lost; the loser
// closes its handle and proceeds with a normal attach. No retry
// needed — the winner's spawn is what both clients end up attached
// to.
func (h *ProtocolHandler) lazySpawnRestoredPTY(ctx context.Context, sess *session.Session, rows, cols uint16, log *slog.Logger) error {
	if h.PTYSpawner == nil {
		return errors.New("daemon not configured for restored sessions")
	}
	if rows == 0 || cols == 0 {
		// Fall back to the session's persisted dimensions.
		savedRows, savedCols := sess.WindowSize()
		if rows == 0 {
			rows = savedRows
		}
		if cols == 0 {
			cols = savedCols
		}
	}
	handle, err := h.PTYSpawner(sess, rows, cols)
	if err != nil {
		return fmt.Errorf("spawn PTY: %w", err)
	}
	if err := sess.AssignPTY(handle); err != nil {
		// Race lost OR session was closed in the meantime. Either
		// way, close our handle so we don't leak the child shell.
		// Closer is the PTY interface's Close method.
		if closer, ok := handle.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		if errors.Is(err, session.ErrSessionHasPTY) {
			// Winner's PTY is in place; attach can proceed.
			return nil
		}
		return err
	}
	// We won the race. The persisted alt-screen Repaint that loadSessionFromDir fed
	// into the model belongs to the now-DEAD app (this is the dead-sidecar path — a
	// fresh shell was just spawned, not a reattach to the surviving sidecar). Reset
	// the model to a clean main-buffer grid BEFORE Pump starts, so
	// InjectAltScreenRepaint won't ship that stale full frame over the new shell's
	// main-buffer prompt (a ghost TUI over a live shell); the fresh shell repopulates
	// the model via Pump.
	sess.ResetScreenModel()
	// Start the pump so output flows.
	go sess.Pump()
	log.InfoContext(ctx, "session.restored.lazy_spawn",
		"session", sess.ID().String(),
		"name_hash", session.NameHash(sess.ID(), sess.Name()),
		"rows", rows,
		"cols", cols,
	)
	log.DebugContext(ctx, "session.restored.lazy_spawn.name",
		"session", sess.ID().String(),
		"name", sess.Name(),
	)
	return nil
}

func (h *ProtocolHandler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// resolveAttach validates the token + session id and consumes the
// token. On failure it sends an AttachAck{ok:false} on the control
// stream and returns the underlying error so the caller can log.
func (h *ProtocolHandler) resolveAttach(att protocol.Attach, ctrl io.Writer, srcIP string) (*session.Session, error) {
	if len(att.Token) != session.AttachTokenLen {
		h.Limiter.RecordBadToken(srcIP)
		_ = sendAttachAck(ctrl, protocol.AttachAck{
			V:   1,
			Err: protocol.AttachErrBadToken,
			Msg: "token length mismatch",
		})
		return nil, errors.New("invalid token length")
	}
	if len(att.SessionID) != session.SessionIDLen {
		// Malformed attach (a legitimate client always sends a fixed-length
		// session id): strike it like the other bad-token paths so a scanner
		// can't probe unbounded behind the accept bucket.
		h.Limiter.RecordBadToken(srcIP)
		_ = sendAttachAck(ctrl, protocol.AttachAck{
			V:   1,
			Err: protocol.AttachErrUnknownSession,
			Msg: "session id length mismatch",
		})
		return nil, errors.New("invalid session id length")
	}
	var tok session.AttachToken
	copy(tok[:], att.Token)
	sess, err := h.Registry.ConsumeAttachToken(tok)
	if err != nil {
		h.Limiter.RecordBadToken(srcIP)
		_ = sendAttachAck(ctrl, protocol.AttachAck{
			V:   1,
			Err: protocol.AttachErrBadToken,
			Msg: err.Error(),
		})
		return nil, err
	}
	// Constant-time SID compare. The win here is small in absolute
	// terms (the registry's map lookup already exposes more timing
	// than the byte compare ever would, and 128 bits of entropy
	// makes guessing not a practical attack surface), but the
	// SECURITY.md self-audit checklist explicitly requires this
	// pattern and it costs us nothing.
	sid := sess.ID()
	if subtle.ConstantTimeCompare(att.SessionID, sid[:]) != 1 {
		// Reaching here means a valid single-use token was consumed but
		// the supplied session id doesn't match it — anomalous for a
		// legitimate client. Strike it like the sibling bad-token paths
		// so the accept limiter accounts for every malformed attach.
		h.Limiter.RecordBadToken(srcIP)
		_ = sendAttachAck(ctrl, protocol.AttachAck{
			V:   1,
			Err: protocol.AttachErrUnknownSession,
			Msg: "session id does not match the token's session",
		})
		return nil, errors.New("session id / token mismatch")
	}
	return sess, nil
}

// readAttach reads the first tagged frame from the stream and
// validates it's a control frame carrying an Attach. Returns
// sentinel errors so the caller can pick the right QUIC application
// error code via errors.Is.
//
// Notably we do NOT include the peer-supplied "got %q" type tag in
// the wrapped error — that string round-trips into the
// CONNECTION_CLOSE reason via closeMsgFor, and we don't echo peer
// bytes there (audit F8).
// maxClientIDLen caps the per-device ClientID hint. A stable UUID is 36 bytes; a
// larger value has no legitimate use and is truncated so a malformed/hostile id can't
// bloat per-session state or logs (the 64 KiB frame cap is the only other bound).
// (Low, security audit v1.7.0.)
const maxClientIDLen = 128

// shouldResetStrandedMouse decides whether an attach should inject the mouse-off
// DECRSTs to clear a mode a dead TUI stranded. Only replay (budget>0) carries the
// stranded mouse-ON, so budget-less CLI attaches skip. Reset when EITHER the
// attach just lazy-spawned a restored session (a fresh shell that can't want
// mouse, and whose fg poller hasn't sampled yet — `fgComm` reads "" for the first
// ~5s, the reboot case) OR the live foreground is a plain shell (a TUI exited on a
// still-up box). A live mouse-using TUI (not restored, fg != shell) is left alone.
func shouldResetStrandedMouse(replayBudget uint64, wasRestored bool, fgComm string) bool {
	if replayBudget == 0 {
		return false
	}
	return wasRestored || isPlainShellComm(fgComm)
}

// isPlainShellComm reports whether a foreground process comm is an interactive
// shell (not a TUI/agent). Only when a session's foreground is a plain shell is
// it safe to inject a mouse-mode reset on attach: a shell never wants mouse
// reporting, whereas a live TUI (htop/vim/claude) would be broken by a blind
// reset. Allowlist (not a TUI denylist) so a node-wrapped agent reported as
// "node" is never mistaken for a shell. Login shells arrive as "-bash".
func isPlainShellComm(comm string) bool {
	switch strings.TrimPrefix(comm, "-") {
	case "bash", "zsh", "sh", "dash", "ash", "fish", "ksh", "mksh", "tcsh", "csh":
		return true
	}
	return false
}

func readAttach(s io.Reader) (protocol.Attach, error) {
	frameType, body, err := protocol.ReadTaggedFrame(s)
	if err != nil {
		return protocol.Attach{}, fmt.Errorf("%w: %v", errAttachBadFrame, err)
	}
	if frameType != protocol.FrameTypeControl {
		return protocol.Attach{}, errAttachWrongFirstFrame
	}
	t, err := protocol.PeekType(body)
	if err != nil {
		return protocol.Attach{}, fmt.Errorf("%w: %v", errAttachBadFrame, err)
	}
	if t != protocol.TypeAttach {
		return protocol.Attach{}, errAttachWrongFirstFrame
	}
	var att protocol.Attach
	if err := protocol.StrictDecMode.Unmarshal(body, &att); err != nil {
		return protocol.Attach{}, fmt.Errorf("%w: %v", errAttachBadFrame, err)
	}
	if len(att.ClientID) > maxClientIDLen {
		att.ClientID = att.ClientID[:maxClientIDLen]
	}
	return att, nil
}

// sendAttachAck encodes a CBOR AttachAck and writes it as a control-
// type tagged frame on the single stream. Used by resolveAttach for
// failure responses where the writeMu serialiser isn't yet in scope.
func sendAttachAck(s io.Writer, ack protocol.AttachAck) error {
	body, err := protocol.MarshalAttachAck(ack)
	if err != nil {
		return err
	}
	return protocol.WriteTaggedFrame(s, protocol.FrameTypeControl, body)
}

// Sentinel errors readAttach returns. Classifying via errors.Is
// rather than substring-matching English strings keeps the
// classification stable when error messages are reformulated
// (audit F9).
var (
	errAttachWrongFirstFrame = errors.New("expected Attach as first control frame")
	errAttachBadFrame        = errors.New("could not decode Attach frame")
)

// errCodeFor maps an attach-handshake error to a QUIC application
// error code. Used only for the connection-close path; AttachAck
// failures use protocol.AttachErr* strings on the wire.
func errCodeFor(err error) uint64 {
	switch {
	case errors.Is(err, errAttachWrongFirstFrame):
		return protocol.ErrStreamWrongOrder
	case errors.Is(err, errAttachBadFrame):
		return protocol.ErrBadFrame
	default:
		return protocol.ErrProtocolViolation
	}
}

// closeMsgFor returns a fixed-string close reason for the given
// error code. Never includes peer-supplied bytes — the close reason
// rides in a CONNECTION_CLOSE frame and a malicious peer could
// otherwise shape its own input back into our outbound diagnostics.
func closeMsgFor(code uint64) string {
	switch code {
	case protocol.ErrStreamWrongOrder:
		return "expected Attach as first control frame"
	case protocol.ErrBadFrame:
		return "control frame decode failed"
	case protocol.ErrProtocolViolation:
		return "protocol violation"
	case protocol.ErrOversizedFrame:
		return "control frame exceeded size limit"
	default:
		return "internal error"
	}
}

// AltScreenReplayCap bounds replay when the session's alt screen is
// active AND the client supplied a ReplayBudget: the alt screen has
// no scrollback, so anything beyond the final frame repaint is
// parsed and discarded by the client — a full ring of TUI repaints
// is pure attach latency (observed: 4.5–6.7s blank console on iOS
// against a long-lived Claude session). 128 KiB ≫ one worst-case
// styled frame (84×44 iPad is tens of KiB). Tunable during the
// v1.6.0 RC. Budget-less clients (mtroam attach/tail) are never
// clamped — full replay stays their contract.
const AltScreenReplayCap = 128 * 1024

// minNudgeRows is the smallest height worth nudging; below it the shrink would produce a
// degenerate 1-row terminal. Lives with the session because NudgeRepaint enforces it.
//
// nudgeWedgeGrace mutes the wedge detector across the two resize arms.
//
// nudgeHoldWindow is how long the PTY stays one row short before being restored.
//
// ★ MEASURED, and load-bearing. A back-to-back nudge with no hold at all does NOT make
// Claude repaint: Ink reads the terminal size when it handles SIGWINCH, and by then the
// size is already restored, so it sees no change and renders nothing. vim and less repaint
// unconditionally and do not care. Against real Claude the floor is between 0 and 15ms —
// the app only needs the intermediate size to be observable, not to survive Ink's ~250ms
// render throttle. 120ms is ~8x that floor, to absorb signal-delivery and scheduling
// latency on a loaded box, and it costs the attach ~120ms in exchange for a screen the
// app itself drew.
const nudgeHoldWindow = 120 * time.Millisecond

// altScreenFooterRows is how many bottom rows the daemon re-emits into the ring
// on an alt-screen attach (the footer block: status footer + separator + prompt
// border + prompt line, with margin). Generous so it covers the event-driven
// bottom region across terminal heights; the rows above it are output-driven and
// reliably already inside the replay window. Re-emit is idempotent, so a too-large
// value only costs a few bytes, never correctness.
const altScreenFooterRows = 8

// computeReplayWindow figures out where on the buffer the replay
// stream should start, given the client's last-acked seq and its
// optional replay budget. Base cases per docs/mtroam-protocol.md
// § 7.3 and § 11.5:
//
//  1. ack >= tail: replay from ack, no truncation
//  2. ack <  tail: replay from tail, truncated=true (some output lost)
//  3. ack >  head: nothing to replay (client claims to have seen
//     bytes we never sent — bug, treat as ack=head)
//
// When budget > 0 (v1.6.0+ clients), the window is additionally
// capped at `budget` bytes back from head — tightened to
// AltScreenReplayCap while the alt screen is active. The cap never
// EXTENDS a window (a small-gap resume replays just the gap); when
// it shrinks one, trunc=true so the client knows output was
// skipped. budget == 0 (missing field) keeps pre-v1.6.0 behavior.
func computeReplayWindow(buf *session.RingBuffer, ack, budget uint64, altActive bool) (start, head uint64, trunc bool) {
	tail := buf.TailSeq()
	head = buf.HeadSeq()
	start = ack
	if start < tail {
		start = tail
		trunc = true
	}
	if start > head {
		start = head
	}
	if budget > 0 {
		capBytes := budget
		if altActive {
			capBytes = min(capBytes, AltScreenReplayCap)
		}
		if head-start > capBytes {
			start = head - capBytes
			trunc = true
		}
	}
	return start, head, trunc
}

// rttNanosFor returns the smoothed RTT from the transport, or 0 when
// the transport doesn't measure RTT (plain TCP) or the smoothing
// window hasn't seeded yet. Callers compare against 0 to decide
// whether to emit RTTNotify frames or carry the value in AttachAck.
func rttNanosFor(c Conn) int64 {
	rtt, ok := c.SmoothedRTT()
	if !ok {
		return 0
	}
	return rtt.Nanoseconds()
}

// fgSinceToNanos converts a foreground-transition timestamp to the
// AttachAck/AgentNotify FgSince wire value. The zero time means
// "unknown" and maps to 0 (omitempty drops it) rather than
// time.Time{}.UnixNano(), which is a large garbage sentinel a client
// would misread as a real instant.
func fgSinceToNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
