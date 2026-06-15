package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// testPTY is a minimal session.PTY implementation that we can push
// bytes into from a test. Mirrors the shape of session-pkg's
// internal fakePTY but lives here because Go test helpers don't
// cross package boundaries. It also implements ForegroundReporter
// (ForegroundComm/ForegroundCwd) so the idle/exit gates can be driven
// from a test via SetFg.
type testPTY struct {
	mu      sync.Mutex
	outBuf  bytes.Buffer
	outCond *sync.Cond
	closed  bool
	fgComm  string
}

func newTestPTY() *testPTY {
	p := &testPTY{}
	p.outCond = sync.NewCond(&p.mu)
	return p
}

func (p *testPTY) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.outBuf.Len() == 0 && !p.closed {
		p.outCond.Wait()
	}
	if p.closed && p.outBuf.Len() == 0 {
		return 0, io.EOF
	}
	return p.outBuf.Read(b)
}

func (p *testPTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("write on closed pty")
	}
	// Discard stdin writes — the sequencer writes here, but for these
	// tests we only assert on the output-direction byte observer.
	return len(b), nil
}

func (p *testPTY) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.outCond.Broadcast()
	return nil
}

func (p *testPTY) SetSize(rows, cols uint16) error { return nil }

// ForegroundComm / ForegroundCwd make testPTY a session.ForegroundReporter
// so sess.ForegroundComm() returns whatever SetFg last set.
func (p *testPTY) ForegroundComm() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fgComm
}

func (p *testPTY) ForegroundCwd() string { return "" }

// SetFg sets the reported foreground command (kernel-truth stand-in).
func (p *testPTY) SetFg(comm string) {
	p.mu.Lock()
	p.fgComm = comm
	p.mu.Unlock()
}

// Push simulates the child process emitting bytes to the PTY's
// slave side, which Pump will read.
func (p *testPTY) Push(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outBuf.Write(b)
	p.outCond.Broadcast()
}

// newTestSession spins up a Session backed by testPTY and starts
// the Pump goroutine. Caller is responsible for sess.Close().
func newTestSession(t *testing.T) (*session.Session, *testPTY) {
	t.Helper()
	pipe := newTestPTY()
	sid, err := session.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	sess, err := session.NewSession(sid, "test", pipe, 24, 80, 0, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	go sess.Pump()
	return sess, pipe
}

// streamUntil pushes a byte to the PTY every 10ms until the returned
// stop func is called — simulates an agent mid-turn (its spinner keeps
// the output stream non-quiescent). 10ms is ~15× tighter than the 150ms
// quiet windows the tests use, so a scheduler hiccup under CI load can't
// fabricate a quiescent gap and flip the result.
func streamUntil(pipe *testPTY) (stop func()) {
	done := make(chan struct{})
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				pipe.Push([]byte("."))
			}
		}
	}()
	return func() { close(done) }
}

// TestWaitForAgentIdle_QuietReturnsTrue: a session producing no output
// is idle, so the gate returns true after the quiet window.
func TestWaitForAgentIdle_QuietReturnsTrue(t *testing.T) {
	sess, _ := newTestSession(t)
	defer func() { _ = sess.Close() }()
	if !waitForAgentIdle(context.Background(), sess, "", 100*time.Millisecond, time.Second, 1) {
		t.Fatal("expected idle (true) on a quiet session")
	}
}

// TestWaitForAgentIdle_BusyTimesOut: continuous output (an in-flight
// turn) keeps the stream non-quiescent, so the gate must time out
// (false) rather than report a false idle — this is the repo-corruption
// safety contract.
func TestWaitForAgentIdle_BusyTimesOut(t *testing.T) {
	sess, pipe := newTestSession(t)
	defer func() { _ = sess.Close() }()
	stop := streamUntil(pipe)
	defer stop()
	if waitForAgentIdle(context.Background(), sess, "", 150*time.Millisecond, 400*time.Millisecond, 1) {
		t.Fatal("expected timeout (false) while output keeps streaming")
	}
}

// TestWaitForAgentIdle_BecomesIdleAfterOutputStops: streaming that stops
// flips the gate to idle once the quiet window elapses.
func TestWaitForAgentIdle_BecomesIdleAfterOutputStops(t *testing.T) {
	sess, pipe := newTestSession(t)
	defer func() { _ = sess.Close() }()
	go func() {
		for i := 0; i < 5; i++ {
			pipe.Push([]byte("x"))
			time.Sleep(30 * time.Millisecond)
		}
	}()
	if !waitForAgentIdle(context.Background(), sess, "", 150*time.Millisecond, 2*time.Second, 1) {
		t.Fatal("expected idle (true) after output stops")
	}
}

// TestWaitForAgentIdle_CancelReturnsFalse: ctx cancellation aborts the
// gate promptly even while the agent is busy.
func TestWaitForAgentIdle_CancelReturnsFalse(t *testing.T) {
	sess, pipe := newTestSession(t)
	defer func() { _ = sess.Close() }()
	stop := streamUntil(pipe)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(80 * time.Millisecond); cancel() }()
	if waitForAgentIdle(ctx, sess, "", time.Second, 5*time.Second, 1) {
		t.Fatal("expected false on ctx cancel")
	}
}

// TestWaitForForegroundLeave_LeavesWhenFgChanges: once the foreground
// command moves off the agent (shell returned), the wait succeeds.
func TestWaitForForegroundLeave_LeavesWhenFgChanges(t *testing.T) {
	sess, pipe := newTestSession(t)
	defer func() { _ = sess.Close() }()
	pipe.SetFg("claude")
	go func() { time.Sleep(120 * time.Millisecond); pipe.SetFg("bash") }()
	if !waitForForegroundLeave(context.Background(), sess, "claude", 2*time.Second) {
		t.Fatal("expected leave (true) when fg moves off the agent")
	}
}

// TestWaitForForegroundLeave_TimesOutWhenStuck: an agent that never
// leaves the foreground (wedged /exit) trips the timeout, so the caller
// escalates to the SIGTERM fallback.
func TestWaitForForegroundLeave_TimesOutWhenStuck(t *testing.T) {
	sess, pipe := newTestSession(t)
	defer func() { _ = sess.Close() }()
	pipe.SetFg("claude")
	if waitForForegroundLeave(context.Background(), sess, "claude", 300*time.Millisecond) {
		t.Fatal("expected timeout (false) while fg stays the agent")
	}
}

// TestWaitForForegroundLeave_UnknownAgentFallsBack: with no fg signal
// ("" agent), the wait can't observe the transition, so it settles and
// reports success (assume exited).
func TestWaitForForegroundLeave_UnknownAgentFallsBack(t *testing.T) {
	sess, _ := newTestSession(t)
	defer func() { _ = sess.Close() }()
	if !waitForForegroundLeave(context.Background(), sess, "", 2*time.Second) {
		t.Fatal("expected true fallback for unknown agent")
	}
}

// TestSleepCtx_HonoursDeadline checks the wait helper stops early on
// ctx cancel. Used in the sequencer's per-stage waits; without this
// a client disconnect mid-recovery would leave goroutines hanging.
func TestSleepCtx_HonoursDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- sleepCtx(ctx, 5*time.Second) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("sleepCtx returned true after cancel; expected false")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("sleepCtx did not return promptly after cancel")
	}
}
