package transport

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// newFgHarness mirrors newHandlerHarness but backs the session with a testPTY
// whose foreground comm is controllable via SetFg, so the attach path's
// mouse-mode sanitization (gated on fg==shell) can be exercised end-to-end.
func newFgHarness(t *testing.T) (*harness, *testPTY) {
	t.Helper()
	c, fp := freshCert(t)
	reg := session.NewRegistry(0, time.Hour, time.Hour, 0)
	id, _ := session.NewSessionID()
	pty := newTestPTY()
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
	return &harness{addr: srv.Addr().String(), fp: fp[:], reg: reg, sess: sess, cleanup: cleanup}, pty
}

// drainReplay reads FrameTypeStdout frames until the stream goes quiet (a read
// deadline lapses), concatenating their payloads — the full replay a reattaching
// client would apply to its terminal.
func drainReplay(t *testing.T, h *harness) []byte {
	t.Helper()
	// A TUI enabled mouse reporting; the DECSETs sit in the scrollback ring with
	// no matching DECRST (the TUI was killed ungracefully).
	if _, err := h.sess.InjectOutput([]byte("$ htop\r\n\x1b[?1000h\x1b[?1006h")); err != nil {
		t.Fatal(err)
	}
	tok, err := h.reg.IssueAttachToken(h.sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	sid := h.sess.ID()
	conn, stream, ack := dialAndAttachFrame(t, h, protocol.Attach{
		V: 1, Token: tok[:], SessionID: sid[:], Rows: 24, Cols: 80, ReplayBudget: 4096,
	})
	defer conn.CloseWithError(0, "")
	defer stream.Close()
	if !ack.OK {
		t.Fatalf("attach failed: %s %s", ack.Err, ack.Msg)
	}

	var replay []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = stream.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		frameType, body, err := protocol.ReadTaggedFrame(stream)
		if err != nil {
			break // no more output → replay drained
		}
		if frameType != protocol.FrameTypeStdout {
			continue
		}
		if _, payload, derr := protocol.DecodeStdoutBody(body); derr == nil {
			replay = append(replay, payload...)
		}
	}
	return replay
}

// The mouse-off DECRSTs the daemon injects on attach when the foreground is a
// plain shell (the TUI/agent that enabled mouse is gone).
var mouseOffMarker = []byte("\x1b[?1000l")

func TestAttachInjectsMouseOffUnderShell(t *testing.T) {
	t.Parallel()
	h, pty := newFgHarness(t)
	defer h.cleanup()
	pty.SetFg("bash") // the TUI is gone; a plain shell is foreground now

	replay := drainReplay(t, h)
	if !bytes.Contains(replay, mouseOffMarker) {
		t.Errorf("shell foreground: expected mouse-off DECRST in replay, not found.\nreplay=%q", replay)
	}
	// Full off-set present, not just ?1000l.
	for _, want := range []string{"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1006l"} {
		if !bytes.Contains(replay, []byte(want)) {
			t.Errorf("shell foreground: missing %q in replay", want)
		}
	}
}

func TestAttachLeavesMouseUnderLiveAgent(t *testing.T) {
	t.Parallel()
	h, pty := newFgHarness(t)
	defer h.cleanup()
	pty.SetFg("claude") // a live agent owns the foreground and wants mouse

	replay := drainReplay(t, h)
	if bytes.Contains(replay, mouseOffMarker) {
		t.Errorf("live agent foreground: daemon must NOT inject mouse-off (would kill live mouse).\nreplay=%q", replay)
	}
}

func TestShouldResetStrandedMouse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		budget      uint64
		wasRestored bool
		fgComm      string
		want        bool
	}{
		{"budgetless CLI attach skips", 0, false, "bash", false},
		{"budgetless even if restored", 0, true, "", false},
		{"live shell foreground resets", 4096, false, "bash", true},
		{"live agent foreground left alone", 4096, false, "claude", false},
		{"restored fresh shell resets even with unknown fg (reboot case)", 4096, true, "", true},
		{"restored still resets when fg not yet a shell", 4096, true, "node", true},
	}
	for _, c := range cases {
		if got := shouldResetStrandedMouse(c.budget, c.wasRestored, c.fgComm); got != c.want {
			t.Errorf("%s: shouldResetStrandedMouse(%d,%v,%q)=%v, want %v",
				c.name, c.budget, c.wasRestored, c.fgComm, got, c.want)
		}
	}
}

func TestIsPlainShellComm(t *testing.T) {
	t.Parallel()
	shells := []string{"bash", "-bash", "zsh", "sh", "dash", "ash", "fish", "ksh", "mksh", "tcsh", "csh"}
	for _, s := range shells {
		if !isPlainShellComm(s) {
			t.Errorf("isPlainShellComm(%q) = false, want true", s)
		}
	}
	nonShells := []string{"claude", "codex", "node", "htop", "vim", "less", "tmux", "python3", "", "basher", "shell"}
	for _, s := range nonShells {
		if isPlainShellComm(s) {
			t.Errorf("isPlainShellComm(%q) = true, want false", s)
		}
	}
}
