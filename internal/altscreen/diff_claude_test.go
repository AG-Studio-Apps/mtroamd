package altscreen

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDiffRealClaude is the coverage the v1.7.8 revert demanded and never got.
//
// That revert (2689eea) pulled the screen-model prime back out of stable because
// it regressed Claude's prompt on cold-start attach, and root-caused it to the
// MODEL rather than the client: a decoded repaint held the horizontal rule but
// no vertical/corner box glyphs — Claude's input box was already incomplete in
// the model. Its closing line was explicit:
//
//	"Do not re-enable the prime until the differential harness covers Claude/Ink."
//
// v1.7.9 re-landed the prime with a resize-dirty guard and a hand-written
// claudeFrame() fixture, but nothing that drives REAL Claude/Ink output through
// tmux as an oracle. This does exactly that, mirroring TestDiffRealVim: capture
// the raw pty bytes Claude emits, feed them to the model, and compare the
// reconstructed grid to `tmux capture-pane` cell for cell.
//
// Ink is the untested class precisely because it does what vim/less/htop do not:
// it bottom-anchors a box-drawn input frame and repaints it differentially, so a
// dropped box glyph survives every test those apps pass.
//
// Sized 42x39 — a phone grid, where the bug was reported — rather than 80x24.
func TestDiffRealClaude(t *testing.T) {
	tmuxPath(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
	if os.Getenv("MT_DIFF_CLAUDE") == "" {
		t.Skip("set MT_DIFF_CLAUDE=1 — launches a real Claude TUI in tmux")
	}
	rows, cols := 39, 42
	// Bespoke driver rather than driveRealApp: Claude shows a trust prompt in a
	// fresh directory and needs many seconds AFTER accepting it before the input
	// box paints, which the shared helper's fixed 400ms tail cannot cover. An
	// earlier version of this test used the helper, captured only the trust
	// dialog, and passed vacuously — the box-glyph check compares absent to
	// absent when neither grid has a box.
	want, raw := driveClaude(t, rows, cols)

	s := New(rows, cols)
	s.Feed(raw)
	t.Logf("captured %d raw bytes, model faithful=%v", len(raw), s.Faithful())

	// Guard against the vacuous pass: if the capture never reached the input box
	// there is nothing here worth asserting.
	realJoined := strings.Join(want, "\n")
	if !strings.Contains(realJoined, "\u2500") {
		t.Fatalf("capture never reached Claude's input box — nothing to compare:\n%s", realJoined)
	}

	// The box glyphs the revert found missing. If Ink drew them, the model must
	// hold them — their absence IS the v1.7.8 regression.
	var modelGrid []string
	for r := 0; r < rows; r++ {
		modelGrid = append(modelGrid, strings.TrimRight(s.rowText(r), " "))
	}
	model := strings.Join(modelGrid, "\n")
	real := realJoined

	for _, g := range []string{"│", "╭", "╰", "─"} {
		inReal, inModel := strings.Contains(real, g), strings.Contains(model, g)
		if inReal != inModel {
			t.Errorf("box glyph %q: tmux=%v model=%v — the v1.7.8 fidelity gap", g, inReal, inModel)
		}
	}

	diverged := 0
	for r := 0; r < rows && r < len(want); r++ {
		if modelGrid[r] != want[r] {
			diverged++
			if diverged <= 6 {
				t.Errorf("row %d diverges:\n  tmux  : %q\n  model : %q", r, want[r], modelGrid[r])
			}
		}
	}
	if diverged == 0 {
		t.Logf("model matches tmux on all %d rows — Claude/Ink fidelity confirmed", rows)
	} else {
		t.Errorf("%d/%d rows diverge from the tmux oracle", diverged, rows)
	}
}

// driveClaude launches Claude in a fresh directory, accepts the trust prompt,
// waits for the Ink input box to paint, and returns (tmux grid, raw pty bytes).
func driveClaude(t *testing.T, rows, cols int) ([]string, []byte) {
	tmux := tmuxPath(t)
	conf := tmuxConf(t)
	// Short socket dir: a unix socket path is capped near 108 bytes and t.TempDir()
	// under a long module path silently overruns it.
	dir, err := os.MkdirTemp("/tmp", "mtd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock, raw := dir+"/s", dir+"/raw.bin"
	name := "claudediff"
	run := func(args ...string) ([]byte, error) {
		return exec.Command(tmux, append([]string{"-S", sock, "-f", conf}, args...)...).CombinedOutput()
	}
	if out, err := run("new-session", "-d", "-s", name,
		"-x", itoa(cols), "-y", itoa(rows), "sh"); err != nil {
		t.Skipf("new-session failed (%v): %s", err, out)
	}
	t.Cleanup(func() { run("kill-server") })
	if out, err := run("pipe-pane", "-t", name, "-o", "cat >> "+raw); err != nil {
		t.Skipf("pipe-pane failed (%v): %s", err, out)
	}
	run("send-keys", "-t", name, "cd $(mktemp -d) && claude", "Enter")
	time.Sleep(18 * time.Second) // Ink's first frame (the trust prompt)
	run("send-keys", "-t", name, "1")
	time.Sleep(2 * time.Second)
	run("send-keys", "-t", name, "Enter")
	time.Sleep(15 * time.Second) // main UI + input box
	rawBytes, err := os.ReadFile(raw)
	if err != nil || len(rawBytes) == 0 {
		t.Skip("no raw bytes captured")
	}
	out, err := run("capture-pane", "-t", name, "-p")
	if err != nil {
		t.Skipf("capture-pane failed (%v): %s", err, out)
	}
	return normalizeGrid(string(out), rows), rawBytes
}

func itoa(v int) string { return strconv.Itoa(v) }
