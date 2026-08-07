package session

import (
	"testing"
	"time"
)

// SuppressUntil must never SHORTEN an existing mute.
//
// The recovery sequencer sets a 30s post-recovery cooldown to mute the false-positive
// storm that `claude --continue` replaying scrollback would otherwise trigger. The
// reattach repaint nudge asks for a 5s mute, and with a plain assignment that silently cut
// 25s off the cooldown and re-exposed the storm.
func TestSuppressUntilNeverShortensAnExistingWindow(t *testing.T) {
	w := &wedgeWatcher{}
	now := time.Now()

	long := now.Add(30 * time.Second)
	w.SuppressUntil(long)

	w.SuppressUntil(now.Add(5 * time.Second)) // the nudge, asking for less
	w.mu.Lock()
	got := w.suppressUntil
	w.mu.Unlock()
	if !got.Equal(long) {
		t.Errorf("a shorter suppression truncated a longer one: got %v, want %v", got, long)
	}

	// A genuinely longer window must still extend it.
	longer := now.Add(60 * time.Second)
	w.SuppressUntil(longer)
	w.mu.Lock()
	got = w.suppressUntil
	w.mu.Unlock()
	if !got.Equal(longer) {
		t.Errorf("a longer suppression did not extend the window: got %v, want %v", got, longer)
	}
}
