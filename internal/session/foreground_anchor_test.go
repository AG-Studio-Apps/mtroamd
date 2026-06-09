package session

import (
	"io"
	"sync"
	"testing"
)

// fgFakePTY is a minimal PTY that also reports a settable foreground
// command + cwd (session.ForegroundReporter). Read returns EOF — these
// tests drive observeForegroundAnchor directly rather than via Pump.
type fgFakePTY struct {
	mu   sync.Mutex
	comm string
	cwd  string
}

func (p *fgFakePTY) Read([]byte) (int, error)    { return 0, io.EOF }
func (p *fgFakePTY) Write(b []byte) (int, error) { return len(b), nil }
func (p *fgFakePTY) Close() error                { return nil }
func (p *fgFakePTY) SetSize(_, _ uint16) error   { return nil }
func (p *fgFakePTY) ForegroundCwd() string       { p.mu.Lock(); defer p.mu.Unlock(); return p.cwd }
func (p *fgFakePTY) ForegroundComm() string      { p.mu.Lock(); defer p.mu.Unlock(); return p.comm }
func (p *fgFakePTY) set(comm, cwd string)        { p.mu.Lock(); p.comm, p.cwd = comm, cwd; p.mu.Unlock() }

// TestObserveForegroundAnchorRingSpace checks that the foreground
// transition anchors are stamped in the ring's HeadSeq space, only
// re-stamp on an actual comm change, and capture the byte position at
// the transition (not before/after).
func TestObserveForegroundAnchorRingSpace(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	pty := &fgFakePTY{}
	s, err := NewSession(id, "", pty, 24, 80, 4096, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Output advances the ring, then claude becomes foreground.
	if _, err := s.buf.Write([]byte("hello")); err != nil { // HeadSeq → 5
		t.Fatal(err)
	}
	pty.set("claude", "/proj")
	s.observeForegroundAnchor()

	t1 := s.ForegroundCommSince()
	seq1 := s.ForegroundSinceSeq()
	if t1.IsZero() {
		t.Error("anchor time is zero after the claude transition")
	}
	if want := s.buf.HeadSeq(); seq1 != want {
		t.Errorf("anchor seq = %d, want HeadSeq %d (ring/client space)", seq1, want)
	}
	if seq1 != 5 {
		t.Errorf("anchor seq = %d, want 5", seq1)
	}

	// More output while still claude: same comm → anchor must NOT move.
	if _, err := s.buf.Write([]byte("world")); err != nil { // HeadSeq → 10
		t.Fatal(err)
	}
	s.observeForegroundAnchor()
	if got := s.ForegroundSinceSeq(); got != seq1 {
		t.Errorf("anchor seq moved on unchanged comm: %d -> %d", seq1, got)
	}
	if got := s.ForegroundCommSince(); !got.Equal(t1) {
		t.Error("anchor time moved on unchanged comm")
	}

	// Foreground changes to bash → re-stamp at the new ring position.
	pty.set("bash", "/proj")
	s.observeForegroundAnchor()
	if got, want := s.ForegroundSinceSeq(), s.buf.HeadSeq(); got != want || got != 10 {
		t.Errorf("anchor seq after change = %d, want re-stamped HeadSeq 10", got)
	}
}
