package transport

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// newStreamHarness mirrors newHandlerHarness but backs the session with the
// generic STREAM backend driven by a toy, non-agent clock producer. It exercises
// the stream attach path end-to-end over real QUIC.
func newStreamHarness(t *testing.T, ticks int) *harness {
	t.Helper()
	c, fp := freshCert(t)
	reg := session.NewRegistry(0, time.Hour, time.Hour, 0)
	id, _ := session.NewSessionID()
	sess, err := session.NewStreamSession(id, "clock", 4096, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(sess); err != nil {
		t.Fatal(err)
	}
	// Toy clock producer: emit N opaque "tick" frames, then close. Nothing
	// agent here — if a clock drives the wire path, the seam is generic.
	go func() {
		for i := 0; i < ticks; i++ {
			sess.PublishFrame([]byte(fmt.Sprintf("tick %d", i)))
		}
		sess.CloseStream()
	}()

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
	return &harness{addr: srv.Addr().String(), fp: fp[:], reg: reg, sess: sess, cleanup: cleanup}
}

// TestProtocolHandlerStreamBackendAttach is the Phase-1 end-to-end gate: a real
// client attaches over QUIC to a stream-backed session, the AttachAck reports
// Backend="stream", and the toy producer's opaque frames arrive as control
// frames in order. Proves the generic stream attach path works on the wire.
func TestProtocolHandlerStreamBackendAttach(t *testing.T) {
	t.Parallel()
	h := newStreamHarness(t, 3)
	defer h.cleanup()

	tok, err := h.reg.IssueAttachToken(h.sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	conn, stream, ack := dialAndAttach(t, h, h.sess.ID(), tok)
	defer conn.CloseWithError(0, "")
	defer stream.Close()

	if !ack.OK {
		t.Fatalf("attach failed: %s %s", ack.Err, ack.Msg)
	}
	if ack.Backend != "stream" {
		t.Errorf("AttachAck.Backend = %q, want %q", ack.Backend, "stream")
	}

	// The toy clock frames arrive as control frames (not stdout). Collect the
	// "tick N" frames, skipping any other control traffic (RTT/fg/etc.).
	var got []string
	deadline := time.Now().Add(3 * time.Second)
	for len(got) < 3 && time.Now().Before(deadline) {
		_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
		frameType, body, rerr := protocol.ReadTaggedFrame(stream)
		if rerr != nil {
			t.Fatalf("read frame: %v", rerr)
		}
		if frameType != protocol.FrameTypeControl {
			continue
		}
		if s := string(body); strings.HasPrefix(s, "tick ") {
			got = append(got, s)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d clock frames, want 3: %v", len(got), got)
	}
	for i, f := range got {
		if want := fmt.Sprintf("tick %d", i); f != want {
			t.Errorf("frame %d = %q, want %q", i, f, want)
		}
	}
}
