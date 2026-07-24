package altscreen

import (
	"os"
	"strings"
	"testing"
)

// TestRepaintRoundTripProdRing is the fidelity gate the differential test was
// MISSING: it validates the Repaint() SERIALIZATION, not just the grid. The
// attach path injects Repaint()'s bytes; a lossy serialization drops characters
// on the client's re-render even when the model grid is perfect. Feed a real
// ring → snapshot grid A → serialize with Repaint() → feed those bytes into a
// fresh model B → assert B's grid == A's grid, cell for cell.
//
//	REPRO_SESSION_DIR=~/.local/share/mtroamd/sessions/<sid> go test \
//	  ./internal/altscreen/ -run TestRepaintRoundTripProdRing -v
func TestRepaintRoundTripProdRing(t *testing.T) {
	dir := os.Getenv("REPRO_SESSION_DIR")
	if dir == "" {
		t.Skip("set REPRO_SESSION_DIR to an mtroamd session dir")
	}
	lin, rows, cols, err := linearizeRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cols=%d rows=%d bytes=%d", cols, rows, len(lin))

	a := New(rows, cols)
	a.Feed(lin)
	frame := a.Repaint()
	t.Logf("Repaint frame = %d bytes", len(frame))

	b := New(rows, cols)
	b.Feed(frame)

	diverged := 0
	for r := 0; r < rows; r++ {
		wa := strings.TrimRight(a.rowText(r), " ")
		wb := strings.TrimRight(b.rowText(r), " ")
		if wa != wb {
			diverged++
			t.Errorf("row %d round-trip diverges:\n  grid A (truth):    %q\n  grid B (reparsed): %q", r, wa, wb)
		}
	}
	if diverged == 0 {
		t.Logf("Repaint round-trips losslessly on all %d rows", rows)
	}
}
