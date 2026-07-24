package altscreen

import (
	"strconv"
	"strings"
	"testing"
)

// claudeFrame is a minimal Claude-like alt screen: conversation up top, a
// prompt + status footer pinned to the BOTTOM rows (where Ink draws them).
func claudeFrame(rows int) string {
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	b.WriteString("\x1b[1;1HTOP conversation line")
	b.WriteString("\x1b[2;1Hmore text")
	b.WriteString("\x1b[" + strconv.Itoa(rows-2) + ";1H\x1b[38;5;37m─── separator\x1b[39m")
	b.WriteString("\x1b[" + strconv.Itoa(rows-1) + ";1H❯ prompt input here")
	b.WriteString("\x1b[" + strconv.Itoa(rows) + ";1H\x1b[38;5;220m⏵⏵ auto mode on\x1b[39m")
	return b.String()
}

func footerRow(s *Screen) int {
	for r := 0; r < s.rows; r++ {
		if strings.Contains(s.rowText(r), "prompt input here") {
			return r
		}
	}
	return -1
}

// TestResizeInvalidatesRedraw is the regression for BOTH prime failure modes:
//   - SHRINK below the footer drops it (lockscreen / keyboard-up).
//   - GROW strands the footer in the middle with blanks below (cold-start
//     reattach at full height after the model was at a keyboard-up height).
//
// In both cases the top-anchored resize leaves the grid NOT matching where the
// app will redraw, so the guard must mark the model unfaithful → the attach
// path falls back to raw replay (which carries the app's post-SIGWINCH repaint)
// rather than injecting a stale/misplaced frame.
func TestResizeInvalidatesRedraw(t *testing.T) {
	// SHRINK past the footer.
	shrink := New(22, 45)
	shrink.Feed([]byte(claudeFrame(22)))
	shrink.Resize(10, 45)
	shrink.Resize(22, 45)
	if shrink.Faithful() {
		t.Error("shrink: guard failed — model still faithful after a lossy shrink")
	}

	// GROW that strands the footer (the cold-start reattach case).
	grow := New(22, 45)
	grow.Feed([]byte(claudeFrame(22)))
	if got := footerRow(grow); got != 20 {
		t.Fatalf("setup: footer at %d, want 20", got)
	}
	grow.Resize(42, 45)
	if fr := footerRow(grow); fr == 40 {
		t.Fatal("grow: footer landed at the real bottom (unexpected)")
	}
	if grow.Faithful() {
		t.Error("grow: guard failed — model still faithful after a grow stranded the footer")
	}
}

// TestNoResizeStaysFaithful: the guard must NOT fire when the geometry is
// unchanged — a same-size reattach is exactly where the prime earns its keep
// (footer aged out of the raw window, no app repaint, model still holds it).
func TestNoResizeStaysFaithful(t *testing.T) {
	s := New(42, 45)
	s.Feed([]byte(claudeFrame(42)))
	s.Resize(42, 45) // no-op
	if !s.Faithful() {
		t.Fatal("no-op resize tripped the guard")
	}
}

// TestResizeGuardSelfHeals: after a resize invalidates the model, the app's
// next full clear + repaint restores fidelity so the prime re-arms.
func TestResizeGuardSelfHeals(t *testing.T) {
	s := New(22, 45)
	s.Feed([]byte(claudeFrame(22)))
	s.Resize(42, 45)
	if s.Faithful() {
		t.Fatal("expected unfaithful after resize")
	}
	s.Feed([]byte(claudeFrame(42))) // app repaints at the new size (ESC[2J + full draw)
	if !s.Faithful() || footerRow(s) != 40 {
		t.Fatalf("recovery: faithful=%v footerRow=%d after repaint", s.Faithful(), footerRow(s))
	}
}
