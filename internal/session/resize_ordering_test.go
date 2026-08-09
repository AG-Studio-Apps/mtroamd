package session

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// ★★ F1: the model must adopt the new geometry BEFORE the PTY is told about it.
//
// SetSize is what raises SIGWINCH, and the app answers it fast. Driving real Claude under
// tmux, the full ED 2 repaint lands ~13 ms after the signal and is the FIRST cell-writing
// output; a token or spinner tick never gets in front of it. Pump feeds that repaint into
// the model on its own screenMu acquisition, so if the model is still at the OLD geometry
// when those bytes land, a complete and correct repaint is written into a grid of the wrong
// size, and the resize that follows top-anchors it. The model then looks settled and
// faithful while its lower rows are blank.
//
// That is the shape of the 2026-08-06 device capture: contentBottom=23 inside a correctly
// sized 39-row client grid, with the cursor already restored to row 36. A clamp cannot
// produce that pairing; this can.
//
// sigwinchPTY makes the race deterministic instead of hoping to lose it: SetSize emits the
// app's repaint and does not return until the model has absorbed it, which is the worst
// case of the real timing (repaint fully delivered before Resize gets to the model).
type sigwinchPTY struct {
	*fakePTY
	onSetSize func(rows, cols uint16)
}

func (p *sigwinchPTY) SetSize(rows, cols uint16) error {
	if err := p.fakePTY.SetSize(rows, cols); err != nil {
		return err
	}
	if p.onSetSize != nil {
		p.onSetSize(rows, cols)
	}
	return nil
}

// markedFrame is a bottom-anchored full-screen repaint carrying a marker unique to the
// geometry it was rendered for, so a test can tell WHICH repaint the model absorbed.
func markedFrame(rows int, marker string) string {
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	b.WriteString("\x1b[1;1HTOP conversation line")
	for r := 2; r <= rows-1; r++ {
		b.WriteString("\x1b[" + strconv.Itoa(r) + ";1Hbody " + strconv.Itoa(r))
	}
	b.WriteString("\x1b[" + strconv.Itoa(rows) + ";1H" + marker + " auto mode on")
	return b.String()
}

func TestModelAdoptsTheNewGeometryBeforeTheSigwinch(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	pty := &sigwinchPTY{fakePTY: newFakePTY()}
	s, err := NewSession(id, "resize-ordering", pty, 24, 80, 1<<20, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = s.Close() }()
	go s.Pump()

	// Settle a full-screen app at 24 rows.
	pty.Push([]byte(markedFrame(24, "AT24")))
	waitForModel(t, s, "settled at 24", func() bool {
		return s.screen.AltActive() && modelFrameHas(s, "AT24")
	})

	// Arm the app: the instant it gets SIGWINCH it emits a complete repaint for the NEW
	// geometry, and we do not let the resize proceed until the model has taken it in.
	pty.onSetSize = func(rows, cols uint16) {
		pty.Push([]byte(markedFrame(int(rows), "AT"+strconv.Itoa(int(rows)))))
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s.screenMu.Lock()
			absorbed := strings.Contains(string(s.screen.Repaint()), "AT"+strconv.Itoa(int(rows)))
			s.screenMu.Unlock()
			if absorbed {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Errorf("the app's repaint for %d rows was never absorbed", rows)
	}

	// The grow the iOS keyboard produces on the way down.
	if err := s.Resize(39, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	s.screenMu.Lock()
	rows, _ := s.screen.Size()
	bottom := modelBottomRow(s)
	hasNew := modelFrameHas(s, "AT39")
	s.screenMu.Unlock()

	if rows != 39 {
		t.Fatalf("model geometry = %d rows, want 39", rows)
	}
	if !hasNew {
		t.Error("the model never absorbed the repaint rendered for the new geometry")
	}
	// ★ THE ASSERTION. With the model resized first, the 39-row repaint is parsed at 39
	// rows and the frame reaches the bottom. With the old order it is parsed at 24, then
	// top-anchored into a 39-row grid, and stops around row 24 with blank rows below —
	// a frame that looks complete to every trust check the daemon has.
	if bottom != 39 {
		t.Errorf("the model's frame reaches row %d of 39: a repaint delivered on SIGWINCH "+
			"was applied at the PRE-resize geometry and then top-anchored, leaving the "+
			"bottom blank (this is the device-capture shape)", bottom)
	}
}

// A same-geometry resize raises no SIGWINCH and must not touch the model, which is what
// keeps the common cold-start reattach on the prime's value path.
func TestSameGeometryResizeLeavesTheModelAlone(t *testing.T) {
	id, _ := NewSessionID()
	pty := &sigwinchPTY{fakePTY: newFakePTY()}
	s, err := NewSession(id, "resize-noop", pty, 24, 80, 1<<20, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = s.Close() }()
	go s.Pump()

	pty.Push([]byte(markedFrame(24, "AT24")))
	waitForModel(t, s, "settled", func() bool { return modelFrameHas(s, "AT24") })
	if _, ok := s.InjectAltScreenRepaint(); !ok {
		t.Fatal("setup: a settled model must be injectable")
	}

	called := false
	pty.onSetSize = func(uint16, uint16) { called = true }
	if err := s.Resize(24, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if !called {
		t.Error("SetSize should still be called for a same-geometry resize (unchanged behaviour)")
	}
	if _, ok := s.InjectAltScreenRepaint(); !ok {
		t.Error("a same-geometry resize must not disturb an injectable model")
	}
}
