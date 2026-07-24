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

// TestResizeStrandsFooterUntilRepaint is the PREMISE of the attach-scoped gate
// (in protocol_handler): a top-anchored resize leaves the grid NOT matching
// where the app will redraw, so the model must NOT be injected on a resized
// attach. A GROW strands Claude's bottom-anchored footer mid-screen; only after
// the app repaints at the new size does the model match again (so a later
// SAME-geometry reattach injects a correct frame — the prime's value case).
func TestResizeStrandsFooterUntilRepaint(t *testing.T) {
	s := New(22, 45)
	s.Feed([]byte(claudeFrame(22)))
	if footerRow(s) != 20 { // input at CUP row 21 (1-based) = index 20
		t.Fatalf("setup: footer at %d, want 20", footerRow(s))
	}

	// Cold-start reattach GROW: model was keyboard-up height, client at full height.
	s.Resize(42, 45)
	if footerRow(s) == 40 {
		t.Fatal("grow unexpectedly placed the footer at the real bottom")
	}
	t.Logf("after grow: footer stranded at row %d (real bottom is 40) → attach MUST skip the inject", footerRow(s))

	// App repaints at the new size (Ink on SIGWINCH). Now the model matches.
	s.Feed([]byte(claudeFrame(42)))
	if footerRow(s) != 40 {
		t.Fatalf("after repaint: footer at %d, want 40 (bottom of 42)", footerRow(s))
	}
}

// TestResizeDirtyGatesInject asserts the invariant the attach-path fix relies
// on. A geometry-changing Resize marks the model resize-dirty, so
// InjectAltScreenRepaint refuses and the footer re-emit skips — no top-anchored
// frame ships (the rc6 regression). The next Feed (the app's SIGWINCH repaint)
// clears it, so a later SAME-geometry reattach injects a correct frame (the
// prime's value case). A same-geometry resize must NOT dirty the model, else
// every cold-start same-geom reattach falls back to raw replay and the prime
// never fires.
func TestResizeDirtyGatesInject(t *testing.T) {
	s := New(22, 45)
	s.Feed([]byte(claudeFrame(22)))
	if s.ResizeDirty() {
		t.Fatal("fresh steady-state model must not be resize-dirty")
	}

	// A same-geometry resize is a no-op — must not mark the model dirty.
	s.Resize(22, 45)
	if s.ResizeDirty() {
		t.Fatal("same-geometry resize must not mark the model dirty (kills the prime's value case)")
	}

	// A real geometry change (cold-start grow) top-anchors the grid and MUST
	// mark it dirty — the attach path refuses to inject until the app repaints.
	s.Resize(42, 45)
	if !s.ResizeDirty() {
		t.Fatal("geometry-changing resize must mark the model dirty")
	}
	if footerRow(s) == 40 {
		t.Fatal("precondition: grow should have stranded the footer, not placed it at the bottom")
	}

	// The app's SIGWINCH repaint arrives; the next Feed clears dirty and the
	// footer lands at the real bottom → injectable again.
	s.Feed([]byte(claudeFrame(42)))
	if s.ResizeDirty() {
		t.Fatal("post-resize Feed (the app repaint) must clear the dirty flag")
	}
	if footerRow(s) != 40 {
		t.Fatalf("after repaint: footer at %d, want 40", footerRow(s))
	}
}
