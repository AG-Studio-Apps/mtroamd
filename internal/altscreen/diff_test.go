package altscreen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Differential testing against tmux 3.4 as an ORACLE.
//
// For a scripted case we write the exact escape-sequence bytes to a file and
// (a) feed them to our Screen and (b) `cat` them inside a detached tmux pane of
// the same size, then compare our reconstructed grid to `tmux capture-pane -p`
// (the visible text grid). Because tmux consumes byte-for-byte the same stream
// we feed our model, any divergence is an emulator bug — this is the strongest
// correctness signal available. SGR fidelity is covered by the round-trip unit
// tests; here we compare the visible text grid cell-for-cell.
//
// For a real app (vim) we enable `tmux pipe-pane` to capture the raw pty bytes
// the app emits, drive it, then feed those exact bytes to our model and compare.

func tmuxPath(t *testing.T) string {
	p, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping differential test")
	}
	return p
}

// tmuxConf returns a config path that turns the status line off (so the whole
// -y height is usable) and pins a UTF-8 256-color terminal.
func tmuxConf(t *testing.T) string {
	dir := t.TempDir()
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g status off\nset -g default-terminal \"tmux-256color\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return conf
}

// tmuxGridFromBytes runs `cat seqfile` in a detached tmux pane of rows×cols and
// returns the visible grid as `rows` right-trimmed lines.
func tmuxGridFromBytes(t *testing.T, seq []byte, rows, cols int) []string {
	tmux := tmuxPath(t)
	conf := tmuxConf(t)
	dir := t.TempDir()
	seqFile := filepath.Join(dir, "seq.bin")
	if err := os.WriteFile(seqFile, seq, 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "srv.sock")
	name := "difftest"
	run := func(args ...string) ([]byte, error) {
		full := append([]string{"-S", sock, "-f", conf}, args...)
		return exec.Command(tmux, full...).CombinedOutput()
	}
	// The pane cats the bytes then sleeps so the drawn screen persists.
	cmd := fmt.Sprintf("cat %s; sleep 30", seqFile)
	if out, err := run("new-session", "-d", "-s", name,
		"-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows), "sh", "-c", cmd); err != nil {
		t.Skipf("tmux new-session failed (%v): %s", err, out)
	}
	defer run("kill-server")
	// Give `cat` time to finish drawing.
	time.Sleep(300 * time.Millisecond)
	out, err := run("capture-pane", "-t", name, "-p")
	if err != nil {
		t.Skipf("tmux capture-pane failed (%v): %s", err, out)
	}
	return normalizeGrid(string(out), rows)
}

// modelGrid feeds seq into a fresh Screen and returns its visible grid as
// `rows` right-trimmed lines.
func modelGrid(seq []byte, rows, cols int) []string {
	s := New(rows, cols)
	s.Feed(seq)
	lines := make([]string, rows)
	for r := 0; r < rows; r++ {
		lines[r] = s.rowText(r)
	}
	return lines
}

// normalizeGrid splits capture output into exactly `rows` right-trimmed lines.
func normalizeGrid(s string, rows int) []string {
	raw := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := make([]string, rows)
	for i := 0; i < rows; i++ {
		if i < len(raw) {
			out[i] = strings.TrimRight(raw[i], " ")
		}
	}
	return out
}

func diffGrids(t *testing.T, name string, want, got []string) int {
	diffs := 0
	for i := range want {
		if want[i] != got[i] {
			diffs++
			t.Errorf("[%s] row %d:\n  tmux : %q\n  model: %q", name, i, want[i], got[i])
		}
	}
	return diffs
}

func TestDiffScriptedCases(t *testing.T) {
	tmuxPath(t) // skip early if no tmux
	rows, cols := 10, 24
	cases := []struct {
		name string
		seq  string
	}{
		{
			// colored box: SGR + cursor addressing.
			"colored-box",
			"\x1b[H\x1b[2J" +
				"\x1b[2;3H\x1b[44m\x1b[37m+--------+\x1b[0m" +
				"\x1b[3;3H\x1b[44m\x1b[37m| \x1b[1;33mHELLO\x1b[0m\x1b[44m\x1b[37m  |\x1b[0m" +
				"\x1b[4;3H\x1b[44m\x1b[37m+--------+\x1b[0m" +
				"\x1b[8;1H\x1b[7m status footer \x1b[0m",
		},
		{
			// scroll-region app pattern: set region, fill, let it scroll.
			"scroll-region",
			"\x1b[H\x1b[2J\x1b[1;1HHEADER\x1b[10;1HFOOTER" +
				"\x1b[2;9r\x1b[2;1H" +
				"one\r\ntwo\r\nthree\r\nfour\r\nfive\r\nsix\r\nseven\r\neight\r\nnine\r\nten",
		},
		{
			// wide chars: CJK + emoji interleaved with ASCII.
			"wide-chars",
			"\x1b[H\x1b[2J\x1b[1;1H\x1b[32m世界\x1b[0m hello" +
				"\x1b[2;1Hemoji:\U0001F600\U0001F680end",
		},
		{
			// last-column pending wrap.
			"pending-wrap",
			"\x1b[H\x1b[2J\x1b[1;1H" + strings.Repeat("A", cols) + "BC" +
				"\x1b[3;1H" + strings.Repeat("x", cols-1) + "Y",
		},
		{
			// alt-screen enter/draw/exit/re-enter cycle.
			"altscreen-cycle",
			"\x1b[H\x1b[2Jmain-line" +
				"\x1b[?1049h\x1b[H\x1b[2J\x1b[3;3Halt content here\x1b[5;1Hmore alt" +
				"\x1b[?1049l" + // back to main
				"\x1b[?1049h\x1b[H\x1b[2J\x1b[1;1Hsecond alt", // re-enter, redraw
		},
		{
			// ECH / ICH / IL sequences.
			"insert-erase",
			"\x1b[H\x1b[2J" +
				"\x1b[1;1Habcdefghij\x1b[1;3H\x1b[3@ZZZ" + // ICH then overwrite
				"\x1b[2;1H0123456789\x1b[2;4H\x1b[2X" + // ECH 2 at col 4
				"\x1b[3;1HKEEP\x1b[4;1HDOWN\x1b[3;1H\x1b[1L\x1b[3;1HINSERTED", // IL
		},
		{
			// DCH / SU / SD / RI.
			"delete-scroll",
			"\x1b[H\x1b[2J" +
				"\x1b[1;1Hwipeout\x1b[1;1H\x1b[3P" + // DCH 3
				"\x1b[3;1Hr3\x1b[4;1Hr4\x1b[5;1Hr5\x1b[2S" + // SU 2 (full screen)
				"\x1b[8;1Hbottom\x1b[8;1H\x1bM\x1bMup", // RI twice from row 8
		},
	}
	total := 0
	for _, c := range cases {
		want := tmuxGridFromBytes(t, []byte(c.seq), rows, cols)
		got := modelGrid([]byte(c.seq), rows, cols)
		total += diffGrids(t, c.name, want, got)
		// The reconstruction must also stay faithful for all of these.
		if _, faithful := Reconstruct([]byte(c.seq), rows, cols); !faithful {
			t.Errorf("[%s] reconstruction went unfaithful", c.name)
		}
	}
	if total == 0 {
		t.Logf("all %d scripted cases matched tmux cell-for-cell", len(cases))
	}
}

// TestDiffRepaintRoundTripsTmux: our Repaint output, fed back through tmux,
// reproduces the same visible grid tmux showed for the original bytes. This is
// the reattach invariant (a synthesized redraw == the live screen), validated
// against the tmux oracle rather than only our own model.
func TestDiffRepaintRoundTripsTmux(t *testing.T) {
	tmuxPath(t)
	rows, cols := 10, 24
	seq := "\x1b[H\x1b[2J\x1b[1;1H\x1b[1;31mTitle\x1b[0m" +
		"\x1b[3;3H\x1b[42m box \x1b[0m\x1b[5;1H世界 wide" +
		"\x1b[10;1H\x1b[7m footer \x1b[0m"
	orig := tmuxGridFromBytes(t, []byte(seq), rows, cols)
	s := New(rows, cols)
	s.Feed([]byte(seq))
	repaint := s.Repaint()
	// Feed the synthesized repaint through tmux from a clean pane.
	round := tmuxGridFromBytes(t, repaint, rows, cols)
	if n := diffGrids(t, "repaint-roundtrip", orig, round); n == 0 {
		t.Logf("repaint reproduced the tmux grid exactly")
	}
}

// driveRealApp launches a real TUI in tmux, captures the raw pty bytes it emits
// via pipe-pane, feeds those exact bytes to our model, and compares the
// reconstructed grid against tmux capture-pane. Returns (wantGrid, rawBytes,
// faithful) or skips on any tmux hiccup. keystrokes are sent after the app has
// had time to draw, then a final capture is compared.
func driveRealApp(t *testing.T, rows, cols int, launch string, keys []string, settle time.Duration) ([]string, []byte, bool) {
	tmux := tmuxPath(t)
	conf := tmuxConf(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "srv.sock")
	raw := filepath.Join(dir, "raw.bin")
	name := "apptest"
	run := func(args ...string) ([]byte, error) {
		full := append([]string{"-S", sock, "-f", conf}, args...)
		return exec.Command(tmux, full...).CombinedOutput()
	}
	if out, err := run("new-session", "-d", "-s", name,
		"-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows), "sh"); err != nil {
		t.Skipf("new-session failed (%v): %s", err, out)
	}
	t.Cleanup(func() { run("kill-server") })
	// Capture raw bytes BEFORE app output; clear so the grid is fully determined
	// by captured bytes; then launch and drive.
	if out, err := run("pipe-pane", "-t", name, "-o", "cat >> "+raw); err != nil {
		t.Skipf("pipe-pane failed (%v): %s", err, out)
	}
	run("send-keys", "-t", name, "clear", "Enter")
	time.Sleep(200 * time.Millisecond)
	run("send-keys", "-t", name, launch, "Enter")
	time.Sleep(settle)
	for _, k := range keys {
		run("send-keys", "-t", name, k)
		time.Sleep(120 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	rawBytes, err := os.ReadFile(raw)
	if err != nil || len(rawBytes) == 0 {
		t.Skip("no raw bytes captured")
	}
	out, err := run("capture-pane", "-t", name, "-p")
	if err != nil {
		t.Skipf("capture-pane failed (%v): %s", err, out)
	}
	s := New(rows, cols)
	s.Feed(rawBytes)
	return normalizeGrid(string(out), rows), rawBytes, s.Faithful()
}

// TestDiffRealVim exercises the full alt-screen + scroll-region + IL/DL
// repertoire a real editor uses.
func TestDiffRealVim(t *testing.T) {
	tmuxPath(t)
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "sample.txt")
	var body strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&body, "line %02d: the quick brown fox jumps\n", i)
	}
	if err := os.WriteFile(file, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, cols := 24, 80
	want, raw, faithful := driveRealApp(t, rows, cols,
		fmt.Sprintf("vim -u NONE -N %s", file),
		[]string{"G", "M", "gg", "20G"}, 1200*time.Millisecond)
	if !faithful {
		t.Errorf("vim reconstruction went unfaithful")
	}
	got := modelGrid(raw, rows, cols)
	if n := diffGrids(t, "real-vim", want, got); n == 0 {
		t.Logf("real vim: grid matched tmux cell-for-cell (%d raw bytes)", len(raw))
	}
}

// TestDiffRealLess pages a file with `less` (alt-screen + scroll region, and it
// stays static once loaded, so the capture is race-free).
func TestDiffRealLess(t *testing.T) {
	tmuxPath(t)
	if _, err := exec.LookPath("less"); err != nil {
		t.Skip("less not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "big.txt")
	var body strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&body, "%03d: lorem ipsum dolor sit amet consectetur\n", i)
	}
	if err := os.WriteFile(file, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, cols := 24, 80
	want, raw, faithful := driveRealApp(t, rows, cols,
		fmt.Sprintf("less %s", file),
		[]string{"Space", "Space", "b", "G", "g"}, 800*time.Millisecond)
	if !faithful {
		t.Errorf("less reconstruction went unfaithful")
	}
	got := modelGrid(raw, rows, cols)
	if n := diffGrids(t, "real-less", want, got); n == 0 {
		t.Logf("real less: grid matched tmux cell-for-cell (%d raw bytes)", len(raw))
	}
}

// TestDiffRealHtop is best-effort: htop redraws continuously, so the raw byte
// stream captured by pipe-pane can lag tmux's live grid by a frame. We assert
// faithfulness (htop must never trip the fallback) and log the cell diff rather
// than failing on a timing-induced frame mismatch.
func TestDiffRealHtop(t *testing.T) {
	tmuxPath(t)
	if _, err := exec.LookPath("htop"); err != nil {
		t.Skip("htop not installed")
	}
	rows, cols := 24, 80
	want, raw, faithful := driveRealApp(t, rows, cols,
		"htop -d 50", nil, 1500*time.Millisecond)
	if !faithful {
		t.Errorf("htop reconstruction went unfaithful (must be emulated, not fall back)")
	}
	got := modelGrid(raw, rows, cols)
	n := diffGrids2(want, got)
	t.Logf("real htop: faithful=%v, %d/%d rows differ (dynamic app; frame-lag tolerated, %d raw bytes)",
		faithful, n, rows, len(raw))
}

// diffGrids2 counts differing rows without failing the test.
func diffGrids2(want, got []string) int {
	n := 0
	for i := range want {
		if want[i] != got[i] {
			n++
		}
	}
	return n
}
