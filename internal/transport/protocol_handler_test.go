package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/quic-go/quic-go"

	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// fakePTY mirrors the in-memory PTY used by the session package's
// tests. Local copy to keep test imports clean.
type fakePTY struct {
	mu      sync.Mutex
	out     bytes.Buffer
	outCond *sync.Cond
	in      bytes.Buffer
	closed  bool
}

func newFakePTY() *fakePTY {
	p := &fakePTY{}
	p.outCond = sync.NewCond(&p.mu)
	return p
}

// ForegroundComm + ForegroundCwd conform fakePTY to
// session.ForegroundReporter so attach tests can assert AttachAck.Fg /
// Cwd plumbing without a real sidecar. (The FgSince/FgSinceSeq anchors
// are session-derived in Pump, not reported by the backend.)
func (p *fakePTY) ForegroundComm() string { return "fakecmd" }
func (p *fakePTY) ForegroundCwd() string  { return "/fake/cwd" }

func (p *fakePTY) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.out.Len() == 0 && !p.closed {
		p.outCond.Wait()
	}
	if p.closed && p.out.Len() == 0 {
		return 0, io.EOF
	}
	return p.out.Read(b)
}

func (p *fakePTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("write on closed pty")
	}
	return p.in.Write(b)
}

func (p *fakePTY) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.outCond.Broadcast()
	return nil
}

func (p *fakePTY) SetSize(rows, cols uint16) error { return nil }

func (p *fakePTY) push(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.Write(b)
	p.outCond.Broadcast()
}

func (p *fakePTY) stdinSeen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.in.String()
}

// runHandlerHarness sets up a Registry + Session + Server +
// ProtocolHandler and returns the listening address, fingerprint,
// and a cleanup func. The test client dials this address.
type harness struct {
	addr    string
	fp      []byte // SHA-256 fingerprint hex unused; tests use pinningClientTLS via the server tests
	reg     *session.Registry
	sess    *session.Session
	pty     *fakePTY
	cleanup func()
}

func newHandlerHarness(t *testing.T) *harness {
	t.Helper()
	c, fp := freshCert(t)
	reg := session.NewRegistry(0, time.Hour, time.Hour, 0)
	id, _ := session.NewSessionID()
	pty := newFakePTY()
	sess, err := session.NewSession(id, "", pty, 24, 80, 4096, 0)
	if err != nil {
		t.Fatal(err)
	}
	go sess.Pump()
	if err := reg.Add(sess); err != nil {
		t.Fatal(err)
	}

	handler := &ProtocolHandler{Registry: reg}
	srv, err := New(Config{Addr: "127.0.0.1:0", Cert: c, Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)

	cleanup := func() {
		cancel()
		_ = srv.Close()
		_ = sess.Close()
		reg.Shutdown()
	}

	return &harness{
		addr:    srv.Addr().String(),
		fp:      fp[:],
		reg:     reg,
		sess:    sess,
		pty:     pty,
		cleanup: cleanup,
	}
}

// dialAndAttach drives the client side of the Attach handshake on
// the single-stream tagged-frame protocol. Returns the QUIC
// connection, the (single) bidi stream, and the AttachAck. All
// subsequent traffic — stdin frames from the test, stdout frames
// from the daemon, control responses — flows over the same stream.
func dialAndAttach(t *testing.T, h *harness, sid session.SessionID, token session.AttachToken) (*quic.Conn, *quic.Stream, protocol.AttachAck) {
	t.Helper()
	return dialAndAttachFrame(t, h, protocol.Attach{
		V:         1,
		Token:     token[:],
		SessionID: sid[:],
		Rows:      24,
		Cols:      80,
	})
}

// dialAndAttachFrame is dialAndAttach with a caller-supplied Attach
// frame, for tests exercising optional fields (Mode, ReplayBudget).
func dialAndAttachFrame(t *testing.T, h *harness, att protocol.Attach) (*quic.Conn, *quic.Stream, protocol.AttachAck) {
	t.Helper()
	var fp [32]byte
	copy(fp[:], h.fp)

	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(dialCtx, h.addr,
		pinningClientTLS(fp, protocol.ALPN),
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	body, err := protocol.MarshalAttach(att)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteTaggedFrame(stream, protocol.FrameTypeControl, body); err != nil {
		t.Fatalf("write Attach: %v", err)
	}

	// Read AttachAck — must be a control-typed tagged frame.
	frameType, ackBody, err := protocol.ReadTaggedFrame(stream)
	if err != nil {
		t.Fatalf("read AttachAck: %v", err)
	}
	if frameType != protocol.FrameTypeControl {
		t.Fatalf("AttachAck frame type = %d, want FrameTypeControl", frameType)
	}
	var ack protocol.AttachAck
	if err := cbor.Unmarshal(ackBody, &ack); err != nil {
		t.Fatalf("decode AttachAck: %v", err)
	}
	return conn, stream, ack
}

// readNextStdoutFrame blocks until the daemon sends the next
// FrameTypeStdout frame on `stream` and returns its decoded
// (seq, payload). Skips any non-stdout frames (e.g. Pong) so tests
// can consume stdout without being confused by control replies.
func readNextStdoutFrame(t *testing.T, stream *quic.Stream) (uint64, []byte) {
	t.Helper()
	for {
		frameType, body, err := protocol.ReadTaggedFrame(stream)
		if err != nil {
			t.Fatalf("read tagged frame: %v", err)
		}
		if frameType != protocol.FrameTypeStdout {
			continue
		}
		seq, payload, err := protocol.DecodeStdoutBody(body)
		if err != nil {
			t.Fatalf("decode stdout body: %v", err)
		}
		return seq, payload
	}
}

func TestProtocolHandlerHappyPath(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	tok, err := h.reg.IssueAttachToken(h.sess.ID())
	if err != nil {
		t.Fatal(err)
	}

	conn, stream, ack := dialAndAttach(t, h, h.sess.ID(), tok)
	defer conn.CloseWithError(0, "")
	defer stream.Close()

	if !ack.OK {
		t.Fatalf("AttachAck.OK = false, err=%q msg=%q", ack.Err, ack.Msg)
	}

	// PTY pushes some output; ensure it arrives as one or more
	// FrameTypeStdout frames on the same stream.
	want := []byte("hello-from-pty")
	h.pty.push(want)

	got := make([]byte, 0, len(want))
	for len(got) < len(want) {
		_, payload := readNextStdoutFrame(t, stream)
		got = append(got, payload...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestProtocolHandlerStdinReachesPTY(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	tok, _ := h.reg.IssueAttachToken(h.sess.ID())
	conn, stream, ack := dialAndAttach(t, h, h.sess.ID(), tok)
	defer conn.CloseWithError(0, "")
	defer stream.Close()
	if !ack.OK {
		t.Fatalf("AttachAck.OK = false: %s %s", ack.Err, ack.Msg)
	}

	// Send stdin as a tagged frame on the same single stream.
	if err := protocol.WriteTaggedFrame(stream, protocol.FrameTypeStdin, []byte("hi\n")); err != nil {
		t.Fatal(err)
	}

	// Wait for the bytes to arrive at the PTY.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.pty.stdinSeen() == "hi\n" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("PTY stdin did not receive client bytes; saw %q", h.pty.stdinSeen())
}

func TestProtocolHandlerRejectsBadToken(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	var bad session.AttachToken // zero-valued, not in registry
	conn, stream, ack := dialAndAttach(t, h, h.sess.ID(), bad)
	defer conn.CloseWithError(0, "")
	defer stream.Close()

	if ack.OK {
		t.Error("AttachAck.OK = true on bogus token")
	}
	if ack.Err != protocol.AttachErrBadToken {
		t.Errorf("Err = %q, want %q", ack.Err, protocol.AttachErrBadToken)
	}
}

func TestProtocolHandlerRejectsMismatchedSessionID(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	tok, _ := h.reg.IssueAttachToken(h.sess.ID())
	// Use a different session_id than the one the token authorises.
	other, _ := session.NewSessionID()
	conn, stream, ack := dialAndAttach(t, h, other, tok)
	defer conn.CloseWithError(0, "")
	defer stream.Close()

	if ack.OK {
		t.Error("AttachAck.OK = true on session_id mismatch")
	}
	if ack.Err != protocol.AttachErrUnknownSession {
		t.Errorf("Err = %q, want %q", ack.Err, protocol.AttachErrUnknownSession)
	}
}

func TestProtocolHandlerReplaysBufferedOutputOnReattach(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	// First attach + drop it, but leave bytes in the buffer.
	tok, _ := h.reg.IssueAttachToken(h.sess.ID())
	conn1, stream1, ack1 := dialAndAttach(t, h, h.sess.ID(), tok)
	if !ack1.OK {
		t.Fatalf("first attach failed: %s %s", ack1.Err, ack1.Msg)
	}
	conn1.CloseWithError(0, "")
	stream1.Close()

	// PTY emits while disconnected — these bytes accumulate in the
	// session's ring buffer.
	missed := []byte("output-while-disconnected")
	h.pty.push(missed)
	// Give Pump a moment to copy into the ring buffer.
	time.Sleep(20 * time.Millisecond)

	// Reattach with last_ack_seq=0 — server should replay everything
	// since the head was advanced past 0.
	tok2, _ := h.reg.IssueAttachToken(h.sess.ID())
	conn2, stream2, ack2 := dialAndAttach(t, h, h.sess.ID(), tok2)
	defer conn2.CloseWithError(0, "")
	defer stream2.Close()

	if !ack2.OK {
		t.Fatalf("reattach failed: %s %s", ack2.Err, ack2.Msg)
	}
	if ack2.BufSeq < uint64(len(missed)) {
		t.Errorf("AttachAck.BufSeq = %d, want ≥ %d", ack2.BufSeq, len(missed))
	}

	// Read stdout frames until we've seen the missed bytes.
	got := make([]byte, 0, len(missed))
	for len(got) < len(missed) {
		_, payload := readNextStdoutFrame(t, stream2)
		got = append(got, payload...)
	}
	if !bytes.Contains(got, missed) {
		t.Errorf("replay missing %q; got %q", missed, got)
	}
}

// TestProtocolHandlerReconstructsAltScreenFooterOnReattach is the
// regression for the bottom-row-drop bug: an alt-screen TUI draws a
// stable footer once, then only animates the active region. A raw
// byte-window replay (small budget) drops the footer because its draw is
// older than the window; the alt-screen reconstruction must put it back.
// We assert the replay STREAM contains the footer text — true only when
// the screen was reconstructed, not when a raw window was shipped.
func TestProtocolHandlerReconstructsAltScreenFooterOnReattach(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	const rows, cols = 8, 24
	const footer = "-- esc to interrupt"
	var out []byte
	// Enter alt screen + clear, then draw the footer ONCE on the last row.
	out = append(out, []byte("\x1b[?1049h\x1b[2J")...)
	out = append(out, []byte("\x1b[8;1H"+footer)...)
	// Spinner on row 3 only: ~720B total stays inside the 4096 ring (footer
	// retained), but pushes the footer-draw past the 256B budget so a raw
	// byte-window replay would miss it.
	for i := 0; i < 40; i++ {
		out = append(out, []byte("\x1b[3;1H\x1b[Kspinning")...)
	}
	h.pty.push(out)
	time.Sleep(50 * time.Millisecond) // let Pump copy to ring + flag alt-screen

	tok, _ := h.reg.IssueAttachToken(h.sess.ID())
	sid := h.sess.ID()
	conn, stream, ack := dialAndAttachFrame(t, h, protocol.Attach{
		V:            1,
		Token:        tok[:],
		SessionID:    sid[:],
		Rows:         rows,
		Cols:         cols,
		ReplayBudget: 256, // smaller than the spinner output → footer-draw is out of a raw window
	})
	defer conn.CloseWithError(0, "")
	defer stream.Close()
	if !ack.OK {
		t.Fatalf("attach failed: %s %s", ack.Err, ack.Msg)
	}

	// Drain the replay window [Start, BufSeq) and assert the footer is in it.
	want := int(ack.BufSeq - ack.Start)
	replay := make([]byte, 0, want)
	for len(replay) < want {
		_, payload := readNextStdoutFrame(t, stream)
		replay = append(replay, payload...)
	}
	if !bytes.Contains(replay, []byte(footer)) {
		t.Fatalf("reconstructed replay missing footer %q (got %d bytes)", footer, len(replay))
	}
}

// TestComputeReplayWindow covers the v1.6.0 budget + alt-screen
// clamp matrix on top of the original three ack cases. Uses a real
// RingBuffer so the seq arithmetic (monotonic byte offsets, tail =
// head-capacity once wrapped) is the genuine article.
func TestComputeReplayWindow(t *testing.T) {
	t.Parallel()

	// 100-byte ring with 250 bytes written: head=250, tail=150.
	wrapped, err := session.NewRingBuffer(100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Write(make([]byte, 250)); err != nil {
		t.Fatal(err)
	}

	// Big ring for the alt-screen cap cases: head = cap + 200_000,
	// nothing dropped (tail=0).
	big, err := session.NewRingBuffer(AltScreenReplayCap + 300_000)
	if err != nil {
		t.Fatal(err)
	}
	bigHead := uint64(AltScreenReplayCap + 200_000)
	if _, err := big.Write(make([]byte, bigHead)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		buf       *session.RingBuffer
		ack       uint64
		budget    uint64
		altActive bool
		wantStart uint64
		wantTrunc bool
	}{
		// Pre-v1.6.0 behavior: budget 0 never clamps.
		{"no-budget fresh attach replays from tail", wrapped, 0, 0, false, 150, true},
		{"no-budget resume from ack", wrapped, 200, 0, false, 200, false},
		{"no-budget ack beyond head clamps to head", wrapped, 999, 0, false, 250, false},
		{"no-budget alt-screen alone never clamps", big, 0, 0, true, 0, false},

		// Budget on the primary screen.
		{"budget wider than window is a no-op", wrapped, 0, 1000, false, 150, true},
		{"budget narrows fresh attach and marks trunc", wrapped, 0, 30, false, 220, true},
		{"budget never extends a small-gap resume", wrapped, 240, 30, false, 240, false},
		{"budget narrows a large-gap resume", wrapped, 160, 30, false, 220, true},

		// Alt screen tightens the cap — but only when a budget came.
		{"alt clamp wins over a large budget", big, 0, bigHead, true, bigHead - AltScreenReplayCap, true},
		{"smaller budget wins over alt cap", big, 0, 1000, true, bigHead - 1000, true},
		{"alt clamp ignored on primary screen", big, 0, bigHead, false, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start, head, trunc := computeReplayWindow(tc.buf, tc.ack, tc.budget, tc.altActive)
			if start != tc.wantStart {
				t.Errorf("start = %d, want %d", start, tc.wantStart)
			}
			if head != tc.buf.HeadSeq() {
				t.Errorf("head = %d, want %d", head, tc.buf.HeadSeq())
			}
			if trunc != tc.wantTrunc {
				t.Errorf("trunc = %v, want %v", trunc, tc.wantTrunc)
			}
		})
	}
}

// TestProtocolHandlerHonorsReplayBudget drives the budget through
// the real attach handshake: fill the buffer past the budget, attach
// with ReplayBudget set, and assert the server starts the replay
// budget-bytes back from head with Trunc set.
func TestProtocolHandlerHonorsReplayBudget(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	// 1 KiB of pre-attach output; ask for the last 100 bytes only.
	h.pty.push(bytes.Repeat([]byte("x"), 1024))
	time.Sleep(20 * time.Millisecond)

	tok, _ := h.reg.IssueAttachToken(h.sess.ID())
	conn, stream, ack := dialAndAttachFrame(t, h, protocol.Attach{
		V:            1,
		Token:        tok[:],
		SessionID:    h.sess.ID().Bytes(),
		Rows:         24,
		Cols:         80,
		ReplayBudget: 100,
	})
	defer conn.CloseWithError(0, "")
	defer stream.Close()

	if !ack.OK {
		t.Fatalf("attach failed: %s %s", ack.Err, ack.Msg)
	}
	if want := ack.BufSeq - 100; ack.Start != want {
		t.Errorf("AttachAck.Start = %d, want %d (head %d - budget 100)", ack.Start, want, ack.BufSeq)
	}
	if !ack.Trunc {
		t.Error("AttachAck.Trunc = false, want true (budget skipped output)")
	}
}


// TestProtocolHandlerAttachAckCarriesFg — the foreground command
// reaches the wire at attach time (v1.6.1+). The harness fakePTY
// reports a canned value.
func TestProtocolHandlerAttachAckCarriesFg(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	tok, _ := h.reg.IssueAttachToken(h.sess.ID())
	conn, stream, ack := dialAndAttach(t, h, h.sess.ID(), tok)
	defer conn.CloseWithError(0, "")
	defer stream.Close()

	if !ack.OK {
		t.Fatalf("attach failed: %s %s", ack.Err, ack.Msg)
	}
	if ack.Fg != "fakecmd" {
		t.Errorf("AttachAck.Fg = %q, want %q", ack.Fg, "fakecmd")
	}
}

// --- exclusive-if-free + Goodbye(replaced) (first-attach-wins, v1.7.0) ---

func TestProtocolHandlerExclusiveIfFree(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()

	// First if-free attach on a free session → granted exclusive.
	tok1, err := h.reg.IssueAttachToken(h.sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	sid := h.sess.ID()
	connA, streamA, ackA := dialAndAttachFrame(t, h, protocol.Attach{
		V: 1, Token: tok1[:], SessionID: sid[:], Rows: 24, Cols: 80,
		Mode: protocol.AttachModeExclusiveIfFree,
	})
	defer connA.CloseWithError(0, "")
	if !ackA.OK {
		t.Fatalf("first attach not OK: err=%q msg=%q", ackA.Err, ackA.Msg)
	}
	if ackA.Mode != protocol.AttachModeExclusive {
		t.Errorf("first ack.Mode = %q, want %q (granted, not requested, is echoed)",
			ackA.Mode, protocol.AttachModeExclusive)
	}

	// Second if-free attach while A holds exclusive → granted readonly,
	// A undisturbed.
	tok2, err := h.reg.IssueAttachToken(h.sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	connB, _, ackB := dialAndAttachFrame(t, h, protocol.Attach{
		V: 1, Token: tok2[:], SessionID: sid[:], Rows: 50, Cols: 24,
		Mode: protocol.AttachModeExclusiveIfFree,
	})
	defer connB.CloseWithError(0, "")
	if !ackB.OK {
		t.Fatalf("second attach not OK: err=%q msg=%q", ackB.Err, ackB.Msg)
	}
	if ackB.Mode != protocol.AttachModeReadonly {
		t.Errorf("second ack.Mode = %q, want %q", ackB.Mode, protocol.AttachModeReadonly)
	}
	if len(ackB.Peers) != 1 || ackB.Peers[0] != protocol.AttachModeExclusive {
		t.Errorf("second ack.Peers = %v, want [exclusive]", ackB.Peers)
	}

	// A's stream must still be live: PTY output keeps flowing to it.
	want := []byte("still-mine")
	h.pty.push(want)
	got := make([]byte, 0, len(want))
	for len(got) < len(want) {
		_, payload := readNextStdoutFrame(t, streamA)
		got = append(got, payload...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("holder stdout after if-free join = %q, want %q", got, want)
	}

	// B's 50×24 Attach dims must NOT have resized the PTY — readonly
	// clients never own geometry (the incident's resize-stomp guard).
	rows, cols := h.sess.WindowSize()
	if rows != 24 || cols != 80 {
		t.Errorf("PTY size after readonly join = %d×%d, want 24×80", rows, cols)
	}
}

func TestProtocolHandlerGoodbyeReplacedOnDisplacement(t *testing.T) {
	t.Parallel()
	h := newHandlerHarness(t)
	defer h.cleanup()
	sid := h.sess.ID()

	tok1, err := h.reg.IssueAttachToken(sid)
	if err != nil {
		t.Fatal(err)
	}
	connA, streamA, ackA := dialAndAttach(t, h, sid, tok1)
	defer connA.CloseWithError(0, "")
	if !ackA.OK {
		t.Fatalf("first attach not OK: err=%q", ackA.Err)
	}

	// Plain-exclusive B displaces A; A must see Goodbye{replaced}
	// on its stream before teardown.
	tok2, err := h.reg.IssueAttachToken(sid)
	if err != nil {
		t.Fatal(err)
	}
	connB, _, ackB := dialAndAttach(t, h, sid, tok2)
	defer connB.CloseWithError(0, "")
	if !ackB.OK {
		t.Fatalf("second attach not OK: err=%q", ackB.Err)
	}

	deadline := time.Now().Add(3 * time.Second)
	_ = streamA.SetReadDeadline(deadline)
	for {
		frameType, body, err := protocol.ReadTaggedFrame(streamA)
		if err != nil {
			t.Fatalf("displaced client's stream died without Goodbye(replaced): %v", err)
		}
		if frameType != protocol.FrameTypeControl {
			continue // drain replayed stdout etc.
		}
		typ, err := protocol.PeekType(body)
		if err != nil || typ != protocol.TypeGoodbye {
			continue
		}
		var g protocol.Goodbye
		if err := protocol.StrictDecMode.Unmarshal(body, &g); err != nil {
			t.Fatalf("decode Goodbye: %v", err)
		}
		if g.Reason != protocol.ReasonReplaced {
			t.Errorf("Goodbye.Reason = %q, want %q", g.Reason, protocol.ReasonReplaced)
		}
		return
	}
}
