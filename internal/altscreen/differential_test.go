package altscreen

import (
	"os"
	"os/exec"
	"strings"
	"testing"

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
		"-x", itoa(cols), "-y", itoa(rows), "cat "+tmp.Name()+"; sleep 60").Run(); err != nil {
		t.Skipf("tmux new-session failed: %v", err)
	}
	exec.Command("tmux", "set", "-t", sess, "status", "off").Run()
	// let tmux drain the pipe
	exec.Command("sh", "-c", "sleep 3").Run()
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}
