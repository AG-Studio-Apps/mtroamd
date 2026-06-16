package altscreen

import (
	"strings"
	"testing"
)

// rowText reads row r as a right-trimmed string (test helper, same package).
func (s *Screen) rowText(r int) string {
	var b strings.Builder
	for c := 0; c < s.cols; c++ {
		b.WriteRune(s.grid[r][c].r)
	}
	return strings.TrimRight(b.String(), " ")
}

func feed(rows, cols int, seqs ...string) *Screen {
	s := New(rows, cols)
	for _, q := range seqs {
		s.Feed([]byte(q))
	}
	return s
}

func TestCursorPositioningAndText(t *testing.T) {
	s := feed(5, 10, "\x1b[3;2HHi")
	if got := s.rowText(2); got != " Hi" {
		t.Fatalf("row2 = %q, want %q", got, " Hi")
	}
}

func TestRelativeCursorDown(t *testing.T) {
	// home, then CUD 4 → row index 4 (the relative walk Claude uses)
	s := feed(6, 10, "\x1b[H\x1b[4Bfoot")
	if got := s.rowText(4); got != "foot" {
		t.Fatalf("row4 = %q, want foot", got)
	}
}

func TestCHAColumnAbsolute(t *testing.T) {
	s := feed(2, 12, "\x1b[1;1Habc\x1b[8Gz") // CHA to col 8
	if got := s.rowText(0); got != "abc    z" {
		t.Fatalf("row0 = %q", got)
	}
}

func TestEraseLineToEnd(t *testing.T) {
	s := feed(3, 10, "\x1b[1;1Habcdef", "\x1b[1;4H\x1b[K") // EL0 from col 4
	if got := s.rowText(0); got != "abc" {
		t.Fatalf("EL0 row0 = %q, want abc", got)
	}
}

func TestEraseDisplay(t *testing.T) {
	s := feed(3, 6, "\x1b[1;1Haaa", "\x1b[2;1Hbbb", "\x1b[2J")
	if s.rowText(0) != "" || s.rowText(1) != "" {
		t.Fatalf("ED2 left content: %q %q", s.rowText(0), s.rowText(1))
	}
}

func TestAutowrap(t *testing.T) {
	s := feed(3, 5, "\x1b[1;1H1234567")
	if s.rowText(0) != "12345" || s.rowText(1) != "67" {
		t.Fatalf("wrap: row0=%q row1=%q", s.rowText(0), s.rowText(1))
	}
}

func TestSGRRoundTrip(t *testing.T) {
	s := feed(2, 12, "\x1b[1;1H\x1b[38;5;174mX\x1b[39m")
	rp := string(s.Repaint())
	if !strings.Contains(rp, "38;5;174") {
		t.Fatalf("repaint dropped the 256-color fg: %q", rp)
	}
}

// TestReconstructRoundTrips: feeding a repaint into a fresh screen yields
// the same visible grid — the invariant the daemon relies on.
func TestReconstructRoundTrips(t *testing.T) {
	rows, cols := 6, 20
	src := "\x1b[H\x1b[2J\x1b[2;3H\x1b[38;5;42mhello\x1b[39m\x1b[6;1Hfooter row"
	a := feed(rows, cols, src)
	b := feed(rows, cols, string(a.Repaint()))
	for r := 0; r < rows; r++ {
		if a.rowText(r) != b.rowText(r) {
			t.Fatalf("row %d differs: %q vs %q", r, a.rowText(r), b.rowText(r))
		}
	}
}

// TestStableFooterSurvivesPartialRedraws is the regression for the bug:
// a footer drawn ONCE must survive reconstruction even when only the top
// of the screen is repainted afterward (Claude's spinner). The daemon's
// old byte-window replay dropped it once the footer-draw aged out.
func TestStableFooterSurvivesPartialRedraws(t *testing.T) {
	rows, cols := 8, 24
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString("\x1b[1;1Hheader line")
	b.WriteString("\x1b[8;1H-- esc to interrupt") // footer, drawn once
	for i := 0; i < 1000; i++ {                   // spinner: only ever row 3
		b.WriteString("\x1b[3;1H\x1b[Kspinner")
	}
	rp, faithful := Reconstruct([]byte(b.String()), rows, cols)
	if !faithful {
		t.Fatal("Claude-style ops should stay faithful")
	}
	out := feed(rows, cols, string(rp))
	if got := out.rowText(7); !strings.Contains(got, "esc to interrupt") {
		t.Fatalf("footer dropped after partial redraws: row8 = %q", got)
	}
	if got := out.rowText(2); !strings.Contains(got, "spinner") {
		t.Fatalf("spinner row not reconstructed: row3 = %q", got)
	}
}

// TestUnfaithfulOnRestructuringOps: content-restructuring ops the model
// doesn't emulate (scroll region, insert/delete line, reverse index) must
// flag the reconstruction unfaithful so the caller keeps the raw replay.
func TestUnfaithfulOnRestructuringOps(t *testing.T) {
	for _, seq := range []string{
		"\x1b[2;5r", // DECSTBM scroll region
		"\x1b[3L",   // insert line
		"\x1b[2M",   // delete line
		"\x1b[1@",   // insert char
		"\x1b[1P",   // delete char
		"\x1b[2S",   // scroll up
		"\x1bM",     // RI reverse index
	} {
		_, faithful := Reconstruct([]byte("\x1b[H\x1b[2Jhi"+seq), 6, 12)
		if faithful {
			t.Errorf("seq %q should be unfaithful", seq)
		}
	}
	// Pure Claude repertoire stays faithful.
	if _, faithful := Reconstruct([]byte("\x1b[H\x1b[2J\x1b[3;1H\x1b[38;5;9mx\x1b[39m\x1b[6;1Hfoot"), 6, 12); !faithful {
		t.Error("Claude-style ops should be faithful")
	}
}

func TestRepaintRowsCursorNeutralAndScoped(t *testing.T) {
	s := feed(6, 12, "\x1b[H\x1b[2J\x1b[3;1Hbody\x1b[6;1H\x1b[38;5;9mfooter\x1b[39m")
	r := string(s.RepaintRows(4, 6)) // bottom 2 rows (indices 4,5 = 1-based 5,6)
	if !strings.HasPrefix(r, "\x1b7") || !strings.HasSuffix(r, "\x1b8") {
		t.Fatalf("not cursor-neutral (ESC7…ESC8): %q", r)
	}
	if strings.Contains(r, "\x1b[2J") {
		t.Fatalf("must not clear the screen: %q", r)
	}
	for _, want := range []string{"\x1b[6;1H", "footer", "38;5;9", "\x1b[K"} {
		if !strings.Contains(r, want) {
			t.Fatalf("footer redraw missing %q: %q", want, r)
		}
	}
	if strings.Contains(r, "body") {
		t.Fatalf("leaked a non-bottom row: %q", r)
	}
}

// TestReconstructBottomRowsRestoresFooter is the v2 core guarantee: the bottom-
// rows redraw, applied onto a screen that has the body but lost the footer,
// restores the footer WITHOUT clobbering the body (cursor-neutral, scoped).
func TestReconstructBottomRowsRestoresFooter(t *testing.T) {
	rows, cols := 8, 24
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J\x1b[8;1H-- esc to interrupt")
	for i := 0; i < 200; i++ {
		b.WriteString("\x1b[3;1H\x1b[Kspin")
	}
	redraw, faithful := ReconstructBottomRows([]byte(b.String()), rows, cols, 4)
	if !faithful {
		t.Fatal("Claude-style ops should be faithful")
	}
	if !strings.Contains(string(redraw), "esc to interrupt") {
		t.Fatalf("bottom-rows redraw missing footer: %q", redraw)
	}
	// A client screen that has the header/body but a blank footer row.
	out := feed(rows, cols, "\x1b[H\x1b[2J\x1b[1;1Hheader body", string(redraw))
	if got := out.rowText(7); !strings.Contains(got, "esc to interrupt") {
		t.Fatalf("footer not restored: row8=%q", got)
	}
	if got := out.rowText(0); !strings.Contains(got, "header body") {
		t.Fatalf("body clobbered: row1=%q", got)
	}
}
