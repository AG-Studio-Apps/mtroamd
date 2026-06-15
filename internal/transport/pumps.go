package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// gracefulCancelBackstop bounds how long a displaced client's
// outputPump goroutine may stay pinned in a blocked Write after the
// graceful stream Close — see outputPump's graceful teardown branch.
const gracefulCancelBackstop = 500 * time.Millisecond

// frameWriter sends one tagged frame on the protocol's single bidi
// stream. The protocol_handler creates this with a mutex so the
// output pump and the read pump's control-response sends don't race
// on quic-go's Stream.Write.
type frameWriter func(t uint8, body []byte) error

// outputPump streams the session's ring buffer onto the wire as
// FrameTypeStdout tagged frames (body = `[u64 BE seq][raw bytes]`).
// It blocks on RingBuffer.WaitForData when the buffer has nothing
// past the last-sent seq, and returns when ctx cancels (typical
// teardown path).
//
// fromSeq is the position to start emitting from — the AttachAck's
// Start field; this is either the client's last_ack_seq, or the
// buffer's tail when ack < tail (truncated replay).
//
// Audit F-D (v0.0.2 review): cancelOnDone is symmetric with the
// readPump's F11 fix. RingBuffer.WaitForData honours ctx, but
// once data is in hand the goroutine can pin in `quic-go` Stream
// write when the peer's flow-control window is full; CancelWrite
// on ctx-cancel unblocks that path so the goroutine exits within
// milliseconds of teardown rather than at MaxIdleTimeout.
// graceful, when non-nil and true at teardown time, switches the
// ctx-cancel hook from CancelWrite (a stream RESET that discards
// queued-but-undelivered frames) to a stream Close (FIN — queued
// data, e.g. the Goodbye{replaced} displacement notice, is still
// delivered reliably). A delayed CancelWrite backstop keeps the
// F-D guarantee that a Write pinned on a dead peer's flow-control
// window can't hold the goroutine past teardown.
func outputPump(ctx context.Context, sess *session.Session, s Conn, write frameWriter, fromSeq uint64, graceful *atomic.Bool) error {
	cancelOnDone(ctx, func() {
		if graceful != nil && graceful.Load() {
			_ = s.Close()
			time.AfterFunc(gracefulCancelBackstop, func() { s.CancelWrite(0) })
			return
		}
		s.CancelWrite(0)
	})
	buf := sess.Buffer()
	if buf == nil {
		return nil
	}
	seq := fromSeq
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, newSeq, _ := buf.ReadSince(seq, protocol.MaxOutputFramePayload)
		if len(data) > 0 {
			body := protocol.EncodeStdoutBody(seq, data)
			if err := write(protocol.FrameTypeStdout, body); err != nil {
				return err
			}
			seq = newSeq
			continue
		}
		// Nothing available — block until more arrives or ctx cancels.
		if _, err := buf.WaitForData(ctx, seq); err != nil {
			return err
		}
	}
}

// readPump reads tagged frames from the single bidi stream and
// dispatches by type:
//
//   - FrameTypeStdin: raw payload streams into the session's PTY,
//     UNLESS the attach is readonly — in which case stdin is
//     silently discarded. We don't tear the connection down on a
//     readonly stdin frame because a misbehaving keystroke
//     shouldn't kick the client off the session.
//   - FrameTypeControl: CBOR-decoded; Ack/Resize/Ping/Goodbye are
//     handled via handleControlFrame (with control-side writes
//     going through the same `write` writer). Resize is also
//     dropped for readonly attaches — the exclusive client owns
//     the PTY size.
//
// quic-go's Read does NOT abort on context cancel; without an
// explicit CancelRead a stuck Read would pin this goroutine until
// QUIC's idle timeout. We watch ctx in a sidecar (audit F11).
func readPump(ctx context.Context, sess *session.Session, s Conn, write frameWriter, mode session.AttachMode, gen uint64) error {
	cancelOnDone(ctx, func() { s.CancelRead(0) })
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frameType, body, err := protocol.ReadTaggedFrame(s)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch frameType {
		case protocol.FrameTypeStdin:
			// Only the CURRENT exclusive client drives the shell. The mode pre-check
			// keeps readonly clients off the lock; IsCurrentExclusive is the LIVE gate
			// (M3) so a displaced client's in-flight stdin can't land in the new
			// owner's PTY.
			if mode != session.AttachExclusive || !sess.IsCurrentExclusive(gen) {
				continue // silently drop
			}
			if len(body) > 0 {
				if _, werr := sess.WriteStdin(body); werr != nil {
					return werr
				}
			}
		case protocol.FrameTypeControl:
			if err := handleControlFrame(sess, body, write, mode, gen); err != nil {
				return err
			}
		default:
			// FrameTypeStdout from the client is a protocol violation
			// (only the server emits stdout). Ignore for forward
			// compat instead of tearing down — the same posture as
			// unknown CBOR control message types.
			continue
		}
	}
}

// handleControlFrame dispatches one CBOR control message read off
// the single stream. Mirrors the previous controlPump body, except
// outbound responses (Pong) go through the shared frameWriter so
// they're serialised against outputPump's writes.
//
// Per the protocol spec, Ack is informational in v0 — we don't trim
// the ring buffer below the ack point yet (the buffer's FIFO drop
// policy already bounds memory). Future versions may use Ack to
// keep the buffer larger when network is healthy and clients keep up.
func handleControlFrame(sess *session.Session, body []byte, write frameWriter, mode session.AttachMode, gen uint64) error {
	t, err := protocol.PeekType(body)
	if err != nil {
		return err
	}
	switch t {
	case protocol.TypeAck:
		// v0: informational only.
		return nil
	case protocol.TypeResize:
		// Only the CURRENT exclusive client changes PTY size — they own
		// geometry. Readonly/passive (and displaced — live check, M3) Resize
		// frames are dropped silently rather than tearing the connection down.
		if mode != session.AttachExclusive || !sess.IsCurrentExclusive(gen) {
			slog.Debug("resize: dropped (non-exclusive client)",
				"sid", sess.ID().String(), "mode", mode)
			return nil
		}
		var m protocol.Resize
		if err := protocol.StrictDecMode.Unmarshal(body, &m); err != nil {
			slog.Warn("resize: malformed CBOR — dropped",
				"sid", sess.ID().String(), "err", err)
			return nil // skip malformed; don't tear the connection down
		}
		// Audit F-I (v0.0.2 review): defense-in-depth daemon-side
		// floor + ceiling on PTY dimensions. The kernel ioctl rejects
		// most pathological values, but a peer-supplied 1×1 or 65535
		// has no legitimate use and clamping here matches the iOS
		// client's own pre-send sanity check. Bounds live in limits.go
		// and are shared with the Attach-frame path in protocol_handler.
		if !dimsInBounds(m.Rows, m.Cols) {
			slog.Warn("resize: out-of-range dimensions — dropped",
				"sid", sess.ID().String(), "rows", m.Rows, "cols", m.Cols)
			return nil
		}
		slog.Info("resize: received from client",
			"sid", sess.ID().String(), "rows", m.Rows, "cols", m.Cols)
		if err := sess.Resize(m.Rows, m.Cols); err != nil {
			slog.Warn("resize: session.Resize returned error",
				"sid", sess.ID().String(), "err", err)
		}
		return nil
	case protocol.TypePing:
		var m protocol.Ping
		if err := protocol.StrictDecMode.Unmarshal(body, &m); err != nil {
			return nil
		}
		pong, err := protocol.MarshalPong(protocol.Pong{Nonce: m.Nonce})
		if err != nil {
			return err
		}
		return write(protocol.FrameTypeControl, pong)
	case protocol.TypeRecover:
		// Only the CURRENT exclusive client can drive recovery — same posture as
		// Resize. The LIVE check (M2/M3) means a displaced client can't fire the
		// destructive /exit + claude --continue (+ possible SIGTERM) at the session
		// the new owner now holds. Readonly / passive / displaced are dropped.
		if mode != session.AttachExclusive || !sess.IsCurrentExclusive(gen) {
			slog.Debug("recover: dropped (non-current-exclusive client)",
				"sid", sess.ID().String(), "mode", mode)
			return nil
		}
		var m protocol.Recover
		if err := protocol.StrictDecMode.Unmarshal(body, &m); err != nil {
			slog.Warn("recover: malformed CBOR — dropped",
				"sid", sess.ID().String(), "err", err)
			return nil
		}
		slog.Info("recover: received request from client",
			"sid", sess.ID().String(),
			"grace_ms", m.GraceMillis,
			"custom_prompt", m.SavePrompt != "")
		ctx, rgen := sess.TryStartRecover()
		go func() {
			defer sess.ClearRecover(rgen)
			runRecover(ctx, sess, m, write, rgen)
		}()
		return nil
	case protocol.TypeGoodbye:
		return io.EOF // signal graceful close to readPump
	default:
		// Unknown control message type — ignore for forward compat.
		return nil
	}
}

// cancelOnDone fires `cancel` when ctx is cancelled. Used by pumps
// to escape blocking quic-go Reads on context cancel.
func cancelOnDone(ctx context.Context, cancel func()) {
	go func() {
		<-ctx.Done()
		cancel()
	}()
}
