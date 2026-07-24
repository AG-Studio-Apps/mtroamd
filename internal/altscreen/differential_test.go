package altscreen

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// linearizeRing rebuilds the daemon ring's linear byte order from a persisted
// session dir (meta.cbor + scrollback.bin).
func linearizeRing(dir string) (data []byte, rows, cols int, err error) {
	mb, err := os.ReadFile(dir + "/meta.cbor")
	if err != nil {
		return nil, 0, 0, err
	}
	var m struct {
		Rows     uint16 `cbor:"rows"`
		Cols     uint16 `cbor:"cols"`
		WritePos int    `cbor:"write_pos"`
		Full     bool   `cbor:"full"`
	}
	if err := cbor.Unmarshal(mb, &m); err != nil {
		return nil, 0, 0, err
	}
	raw, err := os.ReadFile(dir + "/scrollback.bin")
	if err != nil {
		return nil, 0, 0, err
	}
	if m.Full {
		data = append(append([]byte{}, raw[m.WritePos:]...), raw[:m.WritePos]...)
	} else {
		data = raw[:m.WritePos]
	}
	return data, int(m.Rows), int(m.Cols), nil
}

// TestDifferentialAgainstTmux is the fidelity gate the model was missing: it
// renders a real captured session ring through BOTH the model and a real VT
// (tmux, the same ground truth the other tests use) and diffs row-by-row. It is
// env-gated (needs a live mtroamd session dir + tmux) so it stays a diagnostic,
// not a CI test. Extend it with a resize sequence to guard the resize path.
//
//	REPRO_SESSION_DIR=~/.local/share/mtroamd/sessions/<sid> go test \
//	  ./internal/altscreen/ -run TestDifferentialAgainstTmux -v
func TestDifferentialAgainstTmux(t *testing.T) {
	dir := os.Getenv("REPRO_SESSION_DIR")
	if dir == "" {
		t.Skip("set REPRO_SESSION_DIR to an mtroamd session dir")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	lin, rows, cols, err := linearizeRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cols=%d rows=%d bytes=%d", cols, rows, len(lin))

	// Model side.
	s := New(rows, cols)
	s.Feed(lin)
	model := make([]string, rows)
	for r := 0; r < rows; r++ {
		model[r] = strings.TrimRight(s.rowText(r), " ")
	}

	// tmux ground-truth side: cat the same bytes into a pane, capture-pane.
	tmp, err := os.CreateTemp("", "difframe-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.Write(lin)
	tmp.Close()
	sess := "difftest"
	exec.Command("tmux", "kill-session", "-t", sess).Run()
	if err := exec.Command("tmux", "new-session", "-d", "-s", sess,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), "cat "+tmp.Name()+"; sleep 60").Run(); err != nil {
		t.Skipf("tmux new-session failed: %v", err)
	}
	exec.Command("tmux", "set", "-t", sess, "status", "off").Run()
	// let tmux drain the pipe
	time.Sleep(3 * time.Second) // let tmux drain the piped ring before capture
	out, err := exec.Command("tmux", "capture-pane", "-t", sess, "-p").Output()
	exec.Command("tmux", "kill-session", "-t", sess).Run()
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	tm := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

	diverged := 0
	for r := 0; r < rows; r++ {
		want := ""
		if r < len(tm) {
			want = strings.TrimRight(tm[r], " ")
		}
		if model[r] != want {
			diverged++
			t.Errorf("row %d diverges:\n  model: %q\n  tmux : %q", r, model[r], want)
		}
	}
	if diverged == 0 {
		t.Logf("model matches tmux on all %d rows", rows)
	}
}

// TestDifferentialResizeMarksDirty is the production-ring guard for the rc6
// regression. It feeds a REAL captured session ring (a Claude/Ink session, the
// untested class that broke) into the model, then resizes to a different
// geometry — exactly what an attach from a differently-sized client does — and
// asserts the model flags itself resize-dirty. rc6 shipped a top-anchored,
// misplaced frame here because nothing marked the post-resize grid
// untrustworthy; the attach path now refuses to inject a dirty model. Env-gated
// (needs a live mtroamd session dir) like the tmux diff above.
//
//	REPRO_SESSION_DIR=~/.local/share/mtroamd/sessions/<sid> go test \
//	  ./internal/altscreen/ -run TestDifferentialResizeMarksDirty -v
func TestDifferentialResizeMarksDirty(t *testing.T) {
	dir := os.Getenv("REPRO_SESSION_DIR")
	if dir == "" {
		t.Skip("set REPRO_SESSION_DIR to an mtroamd session dir")
	}
	lin, rows, cols, err := linearizeRing(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(rows, cols)
	s.Feed(lin)
	// Feeding a ring never resizes (SIGWINCH is out-of-band), so a steady-state
	// model must be clean — the prime's value case.
	if s.ResizeDirty() {
		t.Fatal("steady-state model after feeding the ring must not be resize-dirty")
	}

	// Cold-start reattach at a taller client: the grow that broke Claude on rc6.
	s.Resize(rows+6, cols)
	if !s.ResizeDirty() {
		t.Fatal("post-resize model must be resize-dirty so the attach path refuses to inject the top-anchored grid")
	}
	t.Logf("ring %dx%d grown to %dx%d → resize-dirty, inject correctly refused", cols, rows, cols, rows+6)

	// The app's SIGWINCH repaint (any output) heals it → injectable again.
	s.Feed([]byte("\x1b[1;1H"))
	if s.ResizeDirty() {
		t.Fatal("output after the resize (the app repaint) must clear the dirty flag")
	}
}
