package transport

import (
	"bytes"
	"testing"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/protocol"
)

// waitAltModel blocks until Pump has fed the screen-model into the alt buffer
// (or the deadline lapses), so the attach paths see a settled model.
func waitAltModel(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if has, alt, _ := h.sess.ScreenState(); has && alt {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("screen-model never reached alt state")
}

func attachAndDrain(t *testing.T, h *harness, rows, cols uint16, budget uint64) []byte {
	t.Helper()
	tok, err := h.reg.IssueAttachToken(h.sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	sid := h.sess.ID()
	conn, stream, ack := dialAndAttachFrame(t, h, protocol.Attach{
		V: 1, Token: tok[:], SessionID: sid[:], Rows: rows, Cols: cols, ReplayBudget: budget,
	})
	defer conn.CloseWithError(0, "")
	defer stream.Close()
	if !ack.OK {
		t.Fatalf("attach failed: %s %s", ack.Err, ack.Msg)
	}
	var replay []byte
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = stream.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		ft, body, err := protocol.ReadTaggedFrame(stream)
		if err != nil {
			break
		}
		if ft != protocol.FrameTypeStdout {
			continue
		}
		if _, payload, derr := protocol.DecodeStdoutBody(body); derr == nil {
			replay = append(replay, payload...)
		}
	}
	return replay
}

// primedAltHarness builds a session on the alt screen with a unique footer
// marker at the bottom row, then ages that footer draw out of a small
// raw-replay budget with non-scrolling spinner filler (row 24 stays intact).
// Returns the harness (caller cleans up). Uses a FRESH session per call so the
// inject in one case can't pollute another's ring.
func primedAltHarness(t *testing.T) *harness {
	t.Helper()
	h := newHandlerHarness(t) // 24x80
	h.pty.push([]byte("\x1b[?1049h\x1b[2J\x1b[24;1HZZFOOTERZZ"))
	h.pty.push(bytes.Repeat([]byte("\x1b[1;1Hspinner-update "), 300)) // ~6KB, no wrap/scroll
	waitAltModel(t, h)
	return h
}

const gateSmallBudget = 512 // < the ~6KB of filler → raw replay excludes the footer draw

// TestAttachInjectsOnSameGeometry: the prime's value case. A same-size reattach
// where the footer has aged out of the raw window must inject the model's
// retained footer.
func TestAttachInjectsOnSameGeometry(t *testing.T) {
	h := primedAltHarness(t)
	defer h.cleanup()
	replay := attachAndDrain(t, h, 24, 80, gateSmallBudget)
	if !bytes.Contains(replay, []byte("ZZFOOTERZZ")) {
		t.Errorf("same-geometry attach: footer marker missing — the prime did not inject its value case (len=%d)", len(replay))
	}
}

// TestAttachSkipsInjectOnResize is the cold-start fix: a resized attach must NOT
// inject the top-anchored (misplaced) model frame — it falls back to raw replay
// (the app's SIGWINCH repaint carries the screen). Fresh session so nothing but
// the raw window (which excludes the aged-out footer) can supply the marker.
func TestAttachSkipsInjectOnResize(t *testing.T) {
	h := primedAltHarness(t)
	defer h.cleanup()
	replay := attachAndDrain(t, h, 30, 80, gateSmallBudget) // GROW 24 → 30
	if bytes.Contains(replay, []byte("ZZFOOTERZZ")) {
		t.Errorf("resized attach: footer marker present — the gate did NOT skip the inject (would ship a misplaced frame)")
	}
}
