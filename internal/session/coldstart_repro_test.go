package session

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// ★★ End-to-end reproduction of the 2026-08-06 cold-start prompt loss.
//
// The model-level tests in internal/altscreen pin the heal RULE. These drive the real
// path instead: bytes through the PTY, through Pump, into the live screen model, and out
// through the actual InjectAltScreenRepaint gate the attach handler calls. That matters
// because the rule has to survive things a direct Feed() never exercises — output split
// across arbitrary chunk boundaries, and the resize landing between chunks.
//
// The failure this reproduces: a grow top-anchors the model and leaves the new bottom
// rows blank. Claude keeps streaming (spinner ticks, tokens) without yet re-anchoring.
// Under the old rule any output healed the resize-dirty latch, so the daemon shipped an
// authoritative full-frame repaint whose bottom rows were empty, and the client rendered
// it faithfully — input box and status bar gone.
//
// Device capture this is modelled on: contentBottom=23 in a correctly sized 39-row client
// grid, cursor already restored to row 36.

// tuiFrame is a bottom-anchored full-screen app frame: content at the top, a separator,
// an input box, and a status row on the LAST line, which is where a real TUI anchors it
// (Claude's "auto mode on", vim's status line, htop's meter).
func tuiFrame(rows int) string {
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	b.WriteString("\x1b[1;1HTOP conversation line")
	b.WriteString("\x1b[2;1Hmore text")
	b.WriteString("\x1b[" + strconv.Itoa(rows-2) + ";1H\x1b[38;5;37m─── separator\x1b[39m")
	b.WriteString("\x1b[" + strconv.Itoa(rows-1) + ";1H❯ prompt input here")
	b.WriteString("\x1b[" + strconv.Itoa(rows) + ";1H\x1b[38;5;220m⏵⏵ auto mode on\x1b[39m")
	return b.String()
}

// waitForModel polls until cond holds or the deadline passes. Pump is asynchronous, so
// every push has to be settled before the next step.
func waitForModel(t *testing.T, s *Session, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.screenMu.Lock()
		ok := cond()
		s.screenMu.Unlock()
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the model: %s", what)
}

// modelBottomRow is the highest 1-based row the model's own Repaint addresses, i.e. the
// bottom of the frame a client would actually receive. Deliberately derived from the
// exported Repaint rather than the grid: it is what ships, so it is what matters.
// Returns 0 when the frame addresses no rows. Caller holds screenMu.
func modelBottomRow(s *Session) int {
	rows, _ := s.screen.Size()
	frame := string(s.screen.Repaint())
	for r := rows; r >= 1; r-- {
		if strings.Contains(frame, "\x1b["+strconv.Itoa(r)+";1H") {
			return r
		}
	}
	return 0
}

// modelFrameHas reports whether the model's Repaint carries a string. Caller holds screenMu.
func modelFrameHas(s *Session, needle string) bool {
	return strings.Contains(string(s.screen.Repaint()), needle)
}

func newPumpedSession(t *testing.T, rows, cols uint16) (*Session, *fakePTY) {
	t.Helper()
	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	pty := newFakePTY()
	s, err := NewSession(id, "coldstart-repro", pty, rows, cols, 1<<20, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	go s.Pump()
	return s, pty
}

// The reproduction proper: partial streaming after a grow must NOT make the daemon ship a
// frame that stops short of the bottom.
func TestColdStartDoesNotShipAFrameMissingItsFooter(t *testing.T) {
	s, pty := newPumpedSession(t, 24, 80)

	// Steady state: the app owns a 24-row alt screen, footer on the last row.
	pty.Push([]byte(tuiFrame(24)))
	waitForModel(t, s, "initial frame absorbed", func() bool {
		return s.screen.AltActive() && modelBottomRow(s) == 24
	})
	if _, ok := s.InjectAltScreenRepaint(); !ok {
		t.Fatal("setup: a settled alt-screen model must be injectable")
	}

	// The client grows (keyboard down / rotation). Top-anchored copy leaves rows 24-38
	// blank, and the app has not re-anchored yet.
	if err := s.Resize(39, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	// Claude keeps streaming while it has not yet repainted. Deliberately split across
	// chunk boundaries, since Pump delivers whatever the kernel hands it.
	for _, chunk := range []string{
		"\x1b[3;1H\xe2\xa0", "\x8b thinking",            // spinner, split mid-rune
		"\x1b[4;1Hstreamed ", "token output",            // a token, split mid-write
		"\x1b[5;1H",                                     // a bare cursor move
	} {
		pty.Push([]byte(chunk))
		time.Sleep(3 * time.Millisecond)
	}
	waitForModel(t, s, "partial output absorbed", func() bool {
		return modelFrameHas(s, "thinking") && modelFrameHas(s, "token output")
	})

	// ★ THE ASSERTION. The model is now 39 rows with content only to row 23. Shipping it
	// as an authoritative frame is the bug: the client would render a screen whose bottom
	// 15 rows are empty, losing the input box and status bar.
	s.screenMu.Lock()
	bottom := modelBottomRow(s)
	dirty := s.screen.ResizeDirty()
	s.screenMu.Unlock()

	if bottom > 24 {
		t.Fatalf("setup is not the broken shape: the frame reaches row %d, want 24", bottom)
	}
	if !dirty {
		t.Error("model reports itself clean while its bottom 15 rows are blank")
	}
	if start, ok := s.InjectAltScreenRepaint(); ok {
		t.Fatalf("DAEMON SHIPPED A FRAME MISSING ITS FOOTER: injected at start=%d with "+
			"a frame reaching only row %d of %d — this is the cold-start prompt loss",
			start, bottom, 39)
	}

	// Now the app actually re-anchors (Ink's SIGWINCH repaint), painting the last row.
	pty.Push([]byte(tuiFrame(39)))
	waitForModel(t, s, "repaint at the new geometry", func() bool {
		return modelBottomRow(s) == 39
	})

	start, ok := s.InjectAltScreenRepaint()
	if !ok {
		t.Fatal("after a full repaint the model MUST be injectable again, or the prime " +
			"never fires for this session and reattach loses the footer")
	}
	buf := s.Buffer()
	if buf == nil {
		t.Fatal("no ring buffer")
	}
	// The frame the client would actually receive has to carry the bottom row.
	frame := readRingFrom(t, buf, start)
	if !strings.Contains(frame, "auto mode on") {
		t.Error("the injected frame is missing the bottom status row")
	}
	if !strings.Contains(frame, "\x1b[39;1H") {
		t.Error("the injected frame never addresses row 39, the last row of the new geometry")
	}
}

// The heal must survive the last row's content arriving split across chunks, which is the
// normal case over a network-fed PTY and something a direct Feed() never exercises.
func TestHealSurvivesAChunkSplitOnTheLastRow(t *testing.T) {
	s, pty := newPumpedSession(t, 24, 80)
	pty.Push([]byte(tuiFrame(24)))
	waitForModel(t, s, "initial frame", func() bool { return s.screen.AltActive() })

	if err := s.Resize(39, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, ok := s.InjectAltScreenRepaint(); ok {
		t.Fatal("a fresh grow must not be injectable")
	}

	// Paint the last row one byte at a time, straddling the CSI and the glyph.
	last := "\x1b[39;1H\x1b[38;5;220m⏵⏵ auto mode on\x1b[39m"
	for i := 0; i < len(last); i++ {
		pty.Push([]byte{last[i]})
		time.Sleep(time.Millisecond)
	}
	waitForModel(t, s, "byte-split last row absorbed", func() bool {
		return modelBottomRow(s) == 39
	})

	if _, ok := s.InjectAltScreenRepaint(); !ok {
		t.Error("a last-row repaint delivered byte-by-byte must still heal the latch")
	}
}

// A resize landing BETWEEN two chunks of the app's repaint must not be healed by the
// tail of that repaint, which was rendered for the OLD geometry.
func TestResizeMidRepaintIsNotHealedByTheStaleTail(t *testing.T) {
	s, pty := newPumpedSession(t, 24, 80)
	pty.Push([]byte(tuiFrame(24)))
	waitForModel(t, s, "initial frame", func() bool { return s.screen.AltActive() })

	// The app starts a repaint for the CURRENT 24-row geometry.
	head := "\x1b[H\x1b[2J\x1b[1;1Hfresh top line"
	pty.Push([]byte(head))
	waitForModel(t, s, "repaint head", func() bool { return modelFrameHas(s, "fresh top line") })

	// The resize lands mid-repaint.
	if err := s.Resize(39, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	// The tail of the OLD repaint arrives: it addresses row 24, the last row of the old
	// geometry, not row 39. It must not count as re-anchoring.
	pty.Push([]byte("\x1b[24;1H⏵⏵ auto mode on"))
	waitForModel(t, s, "stale tail absorbed", func() bool { return modelFrameHas(s, "auto mode on") })

	if _, ok := s.InjectAltScreenRepaint(); ok {
		t.Error("the tail of a repaint rendered for the OLD geometry must not heal the " +
			"latch — it paints row 24, not the new last row")
	}
}

func readRingFrom(t *testing.T, buf *RingBuffer, start uint64) string {
	t.Helper()
	data, _, _ := buf.ReadSince(start, 1<<20)
	return string(data)
}
