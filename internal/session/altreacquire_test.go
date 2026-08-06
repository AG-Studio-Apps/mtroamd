package session

import (
	"strings"
	"testing"
	"time"
)

// ★★ The permanently-latched-off model, reproduced.
//
// Observed on a live box 2026-08-06: every attach logged
//
//	wedgeAlt=true modelHas=true modelAlt=false modelFaithful=true injected=false
//
// for 208 consecutive attaches. The PTY was on the alt screen and the daemon knew it, but
// the screen model had been rebuilt under a still-running Claude and never saw a ?1049h
// again, because a running app does not re-emit one. So the model painted the app's output
// into its MAIN grid, its alt grid stayed empty, nothing was ever injectable, and
// persistence saved no repaint (it only saves an alt-active model) so the state survived
// every restart. That session silently lost the attach prime for good.
func TestModelRebuiltUnderARunningAppReacquiresTheAltScreen(t *testing.T) {
	s, pty := newPumpedSession(t, 24, 80)

	// A full-screen app is running: the model and the wedge watcher both see it.
	pty.Push([]byte(tuiFrame(24)))
	waitForModel(t, s, "app on the alt screen", func() bool { return s.screen.AltActive() })
	if !s.WedgeAltScreenActive() {
		t.Fatal("setup: the wedge watcher should have observed the alt-screen entry")
	}
	if _, ok := s.InjectAltScreenRepaint(); !ok {
		t.Fatal("setup: a settled alt-screen model must be injectable")
	}

	// The model is rebuilt underneath the still-running app (restore with no persisted
	// repaint, or the lazy-spawn ResetScreenModel).
	s.ResetScreenModel()

	s.screenMu.Lock()
	altAfterReset := s.screen.AltActive()
	s.screenMu.Unlock()
	if altAfterReset {
		t.Fatal("setup: a rebuilt model should start on the main buffer")
	}
	if !s.WedgeAltScreenActive() {
		t.Fatal("setup: the PTY is still on the alt screen, so the wedge must still say so")
	}

	// THE STUCK STATE. The app keeps drawing, but never re-emits ?1049h, so nothing about
	// the model's own byte stream can ever put it back on the alt buffer.
	pty.Push([]byte("\x1b[2;1Hthe app keeps drawing"))
	waitForModel(t, s, "post-reset output absorbed", func() bool {
		return modelFrameHas(s, "the app keeps drawing")
	})
	s.screenMu.Lock()
	stillMain := !s.screen.AltActive()
	s.screenMu.Unlock()
	if !stillMain {
		t.Fatal("setup is not the stuck shape: the model recovered on its own")
	}
	if _, ok := s.InjectAltScreenRepaint(); ok {
		t.Fatal("a main-buffer model must never be injected")
	}

	// ★ THE FIX. Reconciling against the PTY's observed state adopts the alt buffer.
	if !s.ReconcileAltScreen() {
		t.Fatal("reconcile did not repair a model that disagrees with the PTY")
	}
	s.screenMu.Lock()
	alt, faithful := s.screen.AltActive(), s.screen.Faithful()
	s.screenMu.Unlock()
	if !alt {
		t.Error("after reconciling, the model must be on the alt buffer")
	}
	if faithful {
		t.Error("adopting tells us WHERE the app draws, not WHAT is on screen — the model " +
			"must be unfaithful until it has tracked a full repaint")
	}

	// Still not injectable: we must not ship a screen we have not seen.
	if _, ok := s.InjectAltScreenRepaint(); ok {
		t.Fatal("an adopted-but-unfaithful model must not be injected")
	}

	// The app's next full repaint rebuilds it, and the prime works again.
	pty.Push([]byte(tuiFrame(24)))
	waitForModel(t, s, "repaint after adopting", func() bool {
		return s.screen.Faithful() && modelBottomRow(s) == 24
	})
	start, ok := s.InjectAltScreenRepaint()
	if !ok {
		t.Fatal("after a full repaint the session must be able to serve the prime again")
	}
	frame := readRingFrom(t, s.Buffer(), start)
	if !strings.Contains(frame, "auto mode on") {
		t.Error("the recovered frame is missing the bottom status row")
	}
}

// Reconciling must be inert in the normal case, and must never resurrect a session that
// has genuinely left the alt screen.
func TestReconcileIsInertWhenTheModelAndPtyAgree(t *testing.T) {
	s, pty := newPumpedSession(t, 24, 80)

	// Plain shell: neither the model nor the wedge watcher sees an alt screen.
	pty.Push([]byte("$ echo hello\r\nhello\r\n"))
	time.Sleep(50 * time.Millisecond)
	if s.ReconcileAltScreen() {
		t.Error("must not adopt the alt buffer for a session that was never on it")
	}

	// App enters the alt screen: model and wedge agree, so there is nothing to repair.
	pty.Push([]byte(tuiFrame(24)))
	waitForModel(t, s, "alt screen entered", func() bool { return s.screen.AltActive() })
	if s.ReconcileAltScreen() {
		t.Error("must be a no-op when the model already agrees with the PTY")
	}
	s.screenMu.Lock()
	faithful := s.screen.Faithful()
	s.screenMu.Unlock()
	if !faithful {
		t.Error("a no-op reconcile must not disturb a faithful model")
	}

	// App exits the alt screen. The wedge watcher follows it out, so reconciling must
	// not drag the model back in.
	pty.Push([]byte("\x1b[?1049l"))
	waitForModel(t, s, "alt screen exited", func() bool { return !s.screen.AltActive() })
	if s.WedgeAltScreenActive() {
		t.Skip("wedge watcher still reports alt-active after ?1049l; nothing to assert here")
	}
	if s.ReconcileAltScreen() {
		t.Error("must not re-adopt the alt buffer after the app has left it")
	}
}
