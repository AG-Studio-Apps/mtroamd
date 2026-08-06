package altscreen

import (
	"strconv"
	"strings"
	"testing"
)

// ★ The 2026-08-06 cold-start regression, reproduced from the device capture.
//
// A grow top-anchors the grid and leaves the new bottom rows blank, so the model is
// marked resize-dirty and must not be injected. The heal USED to be any output at all,
// on the theory that output after a resize is the app responding to SIGWINCH. Claude
// falsifies that: it streams tokens and ticks a spinner continuously, so the first
// unrelated byte cleared the latch while the grown rows were still empty. The daemon
// then shipped an authoritative full frame with a blank bottom, and the client rendered
// it faithfully — the user's input box and status bar were simply gone.
//
// The device capture is the shape asserted here: a correctly-sized 39-row client grid
// reporting contentBottom=23, with the cursor already restored near the bottom.
func TestPartialOutputAfterGrowDoesNotHealTheLatch(t *testing.T) {
	s := New(24, 42)
	s.Feed([]byte(claudeFrame(24)))
	if s.ResizeDirty() {
		t.Fatal("setup: steady-state model must not be resize-dirty")
	}

	// The client grows to full height (keyboard down / rotation).
	s.Resize(39, 42)
	if !s.ResizeDirty() {
		t.Fatal("a geometry-changing grow must mark the model resize-dirty")
	}

	// Claude keeps streaming while it has not yet re-anchored: a spinner tick and a
	// token, both landing in the ORIGINAL top-anchored region. This is the output
	// that used to clear the latch.
	s.Feed([]byte("\x1b[3;1H⠋ thinking"))
	s.Feed([]byte("\x1b[4;1Hsome streamed output"))

	if !s.ResizeDirty() {
		t.Fatal("partial output that does not reach the new bottom must NOT heal the " +
			"latch — this is the bug: the model would be injected with blank bottom rows")
	}

	// Prove the frame really is the broken one, so the assertion above is not
	// vacuous: content stops well short of the bottom of the new geometry.
	last := lastNonBlankRow(s)
	if last >= 38 {
		t.Fatalf("expected the stranded frame to stop short of row 38, got %d", last)
	}
	if last != 23 {
		t.Logf("stranded content ends at row %d (device capture showed 23)", last)
	}

	// Now the real SIGWINCH repaint arrives and paints the last row.
	s.Feed([]byte(claudeFrame(39)))
	if s.ResizeDirty() {
		t.Fatal("a full repaint that paints the last row MUST heal the latch, or the " +
			"prime never fires again for this session")
	}
	if got := lastNonBlankRow(s); got != 38 {
		t.Fatalf("after the repaint content should reach row 38, got %d", got)
	}
}

// A shrink leaves surviving top-anchored content ON the new last row. That content
// proves nothing about whether the app has re-anchored, so the latch must stay set
// until the app actually WRITES there.
func TestShrinkIsNotHealedBySurvivingBottomContent(t *testing.T) {
	s := New(39, 42)
	s.Feed([]byte(claudeFrame(39)))

	// Fill the row that will become the new last row, so it is non-blank purely by
	// survival.
	s.Feed([]byte("\x1b[24;1Hsurviving content on the future last row"))

	s.Resize(24, 42)
	if !s.ResizeDirty() {
		t.Fatal("a geometry-changing shrink must mark the model resize-dirty")
	}
	if lastNonBlankRow(s) != 23 {
		t.Fatalf("setup: the new last row should already carry surviving content, "+
			"lastNonBlank=%d", lastNonBlankRow(s))
	}

	// Unrelated output that does not touch the last row must not heal it.
	s.Feed([]byte("\x1b[2;1Hmore streaming"))
	if !s.ResizeDirty() {
		t.Fatal("surviving content on the last row must not be mistaken for a repaint")
	}

	// The app's real repaint writes the last row.
	s.Feed([]byte(claudeFrame(24)))
	if s.ResizeDirty() {
		t.Fatal("a repaint that writes the last row must heal the latch")
	}
}

// A blank write to the last row is not a repaint. Erasing to the bottom (a common
// prelude to redrawing) must not be mistaken for having drawn there.
func TestBlankLastRowDoesNotHealTheLatch(t *testing.T) {
	s := New(24, 42)
	s.Feed([]byte(claudeFrame(24)))
	s.Resize(39, 42)

	// Move to the last row and erase it, then write spaces there.
	s.Feed([]byte("\x1b[39;1H\x1b[2K"))
	if !s.ResizeDirty() {
		t.Fatal("erasing the last row is not painting it")
	}
	s.Feed([]byte("\x1b[39;1H   "))
	if !s.ResizeDirty() {
		t.Fatal("writing blanks to the last row is not painting it")
	}

	s.Feed([]byte("\x1b[39;1H⏵⏵ auto mode on"))
	if s.ResizeDirty() {
		t.Fatal("real content on the last row must heal the latch")
	}
}

// The healed model must actually be shippable: a Repaint taken after the heal has to
// carry the bottom row, since that is the whole point of the gate.
func TestHealedModelRepaintsToTheBottom(t *testing.T) {
	s := New(24, 42)
	s.Feed([]byte(claudeFrame(24)))
	s.Resize(39, 42)
	s.Feed([]byte(claudeFrame(39)))

	if s.ResizeDirty() {
		t.Fatal("model should have healed")
	}
	rp := string(s.Repaint())
	if !strings.Contains(rp, "auto mode on") {
		t.Error("repaint is missing the bottom status row")
	}
	if !strings.Contains(rp, "\x1b["+strconv.Itoa(39)+";1H") {
		t.Error("repaint never addresses row 39, the last row of the new geometry")
	}
}

func lastNonBlankRow(s *Screen) int {
	for r := s.rows - 1; r >= 0; r-- {
		if s.rowEnd(r) >= 0 {
			return r
		}
	}
	return -1
}
