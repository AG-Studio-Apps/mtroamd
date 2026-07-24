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

func hasFooter(s *Screen) bool {
	for r := 0; r < s.rows; r++ {
		if strings.Contains(s.rowText(r), "prompt input here") {
			return true
		}
	}
	return false
}

// TestLossyShrinkGuard is the regression for the rc6/rc7 Claude-prompt loss.
// A top-anchored resize (resizeGrid) drops the bottom rows on shrink, so a
// lockscreen/keyboard-up cycle discards Claude's bottom prompt. The guard marks
// the model unfaithful in that case so the attach path falls back to raw replay
// (which carries the app's post-SIGWINCH repaint) instead of injecting a
// footer-less full frame. It self-heals on the next full clear.
func TestLossyShrinkGuard(t *testing.T) {
	s := New(22, 45)
	s.Feed([]byte(claudeFrame(22)))
	if !hasFooter(s) || !s.Faithful() {
		t.Fatalf("setup: footer=%v faithful=%v", hasFooter(s), s.Faithful())
	}

	s.Resize(10, 45) // keyboard-up / lockscreen shrink past the prompt row
	s.Resize(22, 45) // reattach grows back

	if hasFooter(s) {
		t.Fatal("expected the top-anchored shrink to drop the bottom prompt")
	}
	if s.Faithful() {
		t.Fatal("GUARD FAILED: model reports faithful after dropping the footer — would inject a broken frame")
	}

	// Self-heal: Claude re-clears + full-repaints (~31 ESC[2J per session).
	s.Feed([]byte(claudeFrame(22)))
	if !s.Faithful() || !hasFooter(s) {
		t.Fatalf("recovery: faithful=%v footer=%v after a full repaint", s.Faithful(), hasFooter(s))
	}
}

// TestNonLossyResizeStaysFaithful: the guard must not fire on a grow or on a
// shrink that only crosses blank rows — the prime should still work in the
// common case.
func TestNonLossyResizeStaysFaithful(t *testing.T) {
	grow := New(22, 45)
	grow.Feed([]byte(claudeFrame(22)))
	grow.Resize(40, 45)
	if !grow.Faithful() {
		t.Fatal("grow tripped the guard")
	}

	blank := New(22, 45)
	blank.Feed([]byte("\x1b[?1049h\x1b[H\x1b[2J\x1b[1;1Htop only"))
	blank.Resize(10, 45) // rows 2..22 are blank → nothing real dropped
	if !blank.Faithful() {
		t.Fatal("shrink over blank rows tripped the guard (false positive)")
	}
}
