package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakePTY is an in-memory PTY for testing. Reads come from outBuf
// (typically pre-loaded by the test or appended to by Push), writes
// go to inBuf so tests can assert what the session sent toward the
// child.
type fakePTY struct {
	mu       sync.Mutex
	outBuf   bytes.Buffer
	outCond  *sync.Cond
	inBuf    bytes.Buffer
	rows     uint16
	cols     uint16
	closed   bool
	closeErr error
}

func newFakePTY() *fakePTY {
	p := &fakePTY{}
	p.outCond = sync.NewCond(&p.mu)
	return p
}

func (p *fakePTY) Read(b []byte) (int, error) {
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

func (p *fakePTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("write on closed pty")
	}
	return p.inBuf.Write(b)
}

func (p *fakePTY) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.outCond.Broadcast()
	return p.closeErr
}

func (p *fakePTY) SetSize(rows, cols uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows, p.cols = rows, cols
	return nil
}

// Push simulates the PTY's child process emitting bytes.
func (p *fakePTY) Push(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outBuf.Write(b)
	p.outCond.Broadcast()
}

func (p *fakePTY) StdinSeen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inBuf.String()
}

func TestNewSessionIDIsRandom(t *testing.T) {
	t.Parallel()
	a, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two consecutive NewSessionID calls returned identical values: %s", a)
	}
}

func TestSessionIDStringRoundTrip(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	s := id.String()
	if len(s) != SessionIDLen*2 {
		t.Errorf("String length = %d, want %d", len(s), SessionIDLen*2)
	}
	parsed, err := ParseSessionID(s)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	if parsed != id {
		t.Errorf("round-trip lost data: original=%v parsed=%v", id, parsed)
	}
}

func TestParseSessionIDRejectsBadInput(t *testing.T) {
	t.Parallel()
	bads := []string{"", "abcd", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "123"}
	for _, in := range bads {
		if _, err := ParseSessionID(in); err == nil {
			t.Errorf("ParseSessionID(%q) returned nil error, want error", in)
		}
	}
}

func TestNewSessionRejectsNilPTY(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	if _, err := NewSession(id, "", nil, 24, 80, 0, 0); err == nil {
		t.Error("NewSession(nil pty) returned nil error")
	}
}

func TestPumpCopiesPTYIntoBuffer(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, err := NewSession(id, "", pty, 24, 80, 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	go s.Pump()

	pty.Push([]byte("hello"))
	pty.Push([]byte(", world"))
	// Close PTY so Pump exits and we can read deterministically.
	pty.Close()

	// Wait for Pump to drain.
	deadline := time.Now().Add(time.Second)
	for !s.Closed() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !s.Closed() {
		t.Fatal("session did not close within timeout")
	}

	// Buffer was nil-ed out by Close (Buffer() returns nil after
	// closure to prevent races with reads against torn-down state).
	// To verify content we have to grab Buffer() before Close —
	// reach into the unexported field for testing.
	if got := bytesAccumulated(s); !bytes.Equal(got, []byte("hello, world")) {
		t.Errorf("accumulated = %q, want %q", got, "hello, world")
	}
}

// bytesAccumulated grabs the buffer's full contents directly via the
// unexported field. Used only by tests that have closed the session.
func bytesAccumulated(s *Session) []byte {
	if s.buf == nil {
		return nil
	}
	data, _, _ := s.buf.ReadSince(0, -1)
	return data
}

func TestWriteStdinReachesPTY(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	if _, err := s.WriteStdin([]byte("ls\n")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	if got := pty.StdinSeen(); got != "ls\n" {
		t.Errorf("PTY stdin = %q, want %q", got, "ls\n")
	}
}

func TestResizeUpdatesPTYAndState(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	if err := s.Resize(40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	rows, cols := s.WindowSize()
	if rows != 40 || cols != 120 {
		t.Errorf("WindowSize = %d×%d, want 40×120", rows, cols)
	}
	pty.mu.Lock()
	pr, pc := pty.rows, pty.cols
	pty.mu.Unlock()
	if pr != 40 || pc != 120 {
		t.Errorf("PTY size = %d×%d, want 40×120", pr, pc)
	}
}

func TestAcquireDisplacesPriorAttach(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	parent := context.Background()

	first, gen1, _, err := s.Acquire(parent, AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if gen1 == 0 {
		t.Error("first generation should be > 0")
	}
	if first.Err() != nil {
		t.Error("first attach context cancelled prematurely")
	}

	second, gen2, _, err := s.Acquire(parent, AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if gen2 == gen1 {
		t.Error("second generation should differ from first")
	}
	// First should now be cancelled by the displacement.
	if first.Err() == nil {
		t.Error("first attach context not cancelled when second arrived")
	}
	if second.Err() != nil {
		t.Error("second attach context cancelled prematurely")
	}
}

func TestReleaseDoesNotClearWhenDisplaced(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	parent := context.Background()

	_, gen1, _, _ := s.Acquire(parent, AttachExclusive)
	second, _, _, _ := s.Acquire(parent, AttachExclusive)
	// Old attach calls Release after seeing its ctx cancelled. With
	// the generation counter, this no-ops cleanly.
	s.Release(gen1)
	// Second attach should still be active.
	if second.Err() != nil {
		t.Error("second attach was lost after the first called Release")
	}
}

func TestReleaseStaleGenerationIsNoOp(t *testing.T) {
	t.Parallel()
	// Even if the parent ctx was independently cancelled (e.g.,
	// daemon-wide shutdown), Release(staleGen) must not touch the
	// new active slot. The previous ctx-error-as-identity heuristic
	// got this wrong.
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	parent, cancel := context.WithCancel(context.Background())
	_, gen1, _, _ := s.Acquire(parent, AttachExclusive)
	_, gen2, _, _ := s.Acquire(parent, AttachExclusive)
	cancel() // cancel the shared parent — both ctxs now have Err()
	// First's Release with the OLD gen must not clear gen2's slot.
	s.Release(gen1)
	// Inspect: activeCancel must still be set (a third Acquire
	// should still trigger displacement).
	_, gen3, _, _ := s.Acquire(context.Background(), AttachExclusive)
	if gen3 == gen2 {
		t.Error("third Acquire didn't increment generation; second's cancel was prematurely cleared")
	}
}

// TestAcquireReadonlyDoesNotDisplace: a readonly attach must not
// cancel any existing client (exclusive or readonly). Multiple
// readonly + one exclusive should coexist.
func TestAcquireReadonlyDoesNotDisplace(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	exclCtx, exclGen, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	roCtx, roGen, _, err := s.Acquire(context.Background(), AttachReadonly)
	if err != nil {
		t.Fatal(err)
	}

	// Neither should be cancelled.
	if exclCtx.Err() != nil {
		t.Error("exclusive context cancelled by readonly Acquire")
	}
	if roCtx.Err() != nil {
		t.Error("readonly context cancelled at acquire time")
	}
	// PeerModes(roGen) should report the exclusive client.
	peers := s.PeerModes(roGen)
	if len(peers) != 1 || peers[0] != "exclusive" {
		t.Errorf("readonly's peers = %v, want [exclusive]", peers)
	}
	// PeerModes(exclGen) should report the readonly.
	peers2 := s.PeerModes(exclGen)
	if len(peers2) != 1 || peers2[0] != "readonly" {
		t.Errorf("exclusive's peers = %v, want [readonly]", peers2)
	}
}

// TestAcquireExclusiveDisplacesPriorExclusive: a new exclusive
// attach must cancel any prior exclusive client but leave readonly
// clients alone.
func TestAcquireExclusiveDisplacesPriorExclusive(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	first, _, _, _ := s.Acquire(context.Background(), AttachExclusive)
	roCtx, _, _, _ := s.Acquire(context.Background(), AttachReadonly)

	// Now displace the exclusive.
	second, _, _, _ := s.Acquire(context.Background(), AttachExclusive)

	// First (displaced exclusive) must be cancelled.
	select {
	case <-first.Done():
		// good
	case <-time.After(100 * time.Millisecond):
		t.Error("displaced exclusive context not cancelled within 100ms")
	}
	// Readonly must still be alive.
	if roCtx.Err() != nil {
		t.Error("readonly context cancelled by exclusive replacement")
	}
	// Second exclusive must be alive.
	if second.Err() != nil {
		t.Error("new exclusive context already cancelled")
	}
}

// TestAcquireMultipleReadonlyCoexist: readonly clients accumulate.
// Two readonly Acquire calls leave both alive.
func TestAcquireMultipleReadonlyCoexist(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	a, genA, _, _ := s.Acquire(context.Background(), AttachReadonly)
	b, _, _, _ := s.Acquire(context.Background(), AttachReadonly)
	if a.Err() != nil || b.Err() != nil {
		t.Error("readonly Acquire cancelled a peer")
	}
	if peers := s.PeerModes(genA); len(peers) != 1 || peers[0] != "readonly" {
		t.Errorf("PeerModes after 2 readonly = %v", peers)
	}
}

// TestAcquirePassiveDoesNotDisplace: a new passive Acquire must
// neither cancel exclusive nor readonly co-attachers. Passive is the
// invisible-tap mode.
func TestAcquirePassiveDoesNotDisplace(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	exclCtx, _, _, _ := s.Acquire(context.Background(), AttachExclusive)
	roCtx, _, _, _ := s.Acquire(context.Background(), AttachReadonly)
	passCtx, _, _, err := s.Acquire(context.Background(), AttachPassive)
	if err != nil {
		t.Fatal(err)
	}
	if exclCtx.Err() != nil {
		t.Error("exclusive cancelled by passive Acquire")
	}
	if roCtx.Err() != nil {
		t.Error("readonly cancelled by passive Acquire")
	}
	if passCtx.Err() != nil {
		t.Error("passive context cancelled at acquire time")
	}
}

// TestAcquirePassiveInvisibleInAttachedModes: passive attachers must
// NOT appear in AttachedModes() or PeerModes(). The whole point of
// the mode is that other clients can't see the tap.
func TestAcquirePassiveInvisibleInAttachedModes(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	_, exclGen, _, _ := s.Acquire(context.Background(), AttachExclusive)
	_, _, _, _ = s.Acquire(context.Background(), AttachPassive)
	_, _, _, _ = s.Acquire(context.Background(), AttachPassive)

	modes := s.AttachedModes()
	if len(modes) != 1 || modes[0] != "exclusive" {
		t.Errorf("AttachedModes with 2 passive + 1 exclusive = %v, want [exclusive]", modes)
	}
	peers := s.PeerModes(exclGen)
	if len(peers) != 0 {
		t.Errorf("PeerModes(exclusive) sees passive: %v, want []", peers)
	}
}

// TestAcquirePassiveCapEnforced: MaxPassivePerSession concurrent
// passive attaches succeed; the next one returns ErrPassiveCapacity.
func TestAcquirePassiveCapEnforced(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	gens := make([]uint64, 0, MaxPassivePerSession)
	for i := 0; i < MaxPassivePerSession; i++ {
		_, g, _, err := s.Acquire(context.Background(), AttachPassive)
		if err != nil {
			t.Fatalf("passive #%d unexpectedly failed: %v", i, err)
		}
		gens = append(gens, g)
	}
	_, _, _, err := s.Acquire(context.Background(), AttachPassive)
	if !errors.Is(err, ErrPassiveCapacity) {
		t.Errorf("passive overflow err = %v, want ErrPassiveCapacity", err)
	}
	// Releasing one should free up a slot.
	s.Release(gens[0])
	_, _, _, err = s.Acquire(context.Background(), AttachPassive)
	if err != nil {
		t.Errorf("passive after release unexpectedly failed: %v", err)
	}
}

// TestAcquireReadonlyCapEnforced: MaxReadonlyPerSession concurrent
// read-only attaches succeed; the next returns ErrReadonlyCapacity,
// and releasing one frees a slot.
func TestAcquireReadonlyCapEnforced(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	gens := make([]uint64, 0, MaxReadonlyPerSession)
	for i := 0; i < MaxReadonlyPerSession; i++ {
		_, g, _, err := s.Acquire(context.Background(), AttachReadonly)
		if err != nil {
			t.Fatalf("readonly #%d unexpectedly failed: %v", i, err)
		}
		gens = append(gens, g)
	}
	if _, _, _, err := s.Acquire(context.Background(), AttachReadonly); !errors.Is(err, ErrReadonlyCapacity) {
		t.Errorf("readonly overflow err = %v, want ErrReadonlyCapacity", err)
	}
	// An exclusive attach is NOT a read-only client and must still be
	// admitted at the read-only cap (it lives in the same slice but is
	// bounded by displacement, not the read-only count).
	if _, _, _, err := s.Acquire(context.Background(), AttachExclusive); err != nil {
		t.Errorf("exclusive at readonly cap unexpectedly failed: %v", err)
	}
	// Releasing a read-only client frees a read-only slot.
	s.Release(gens[0])
	if _, _, _, err := s.Acquire(context.Background(), AttachReadonly); err != nil {
		t.Errorf("readonly after release unexpectedly failed: %v", err)
	}
}

// TestExclusiveDoesNotDisplacePassive: replacing the exclusive client
// must leave passive watchers intact (they're invisible by design;
// turnover invisibility cuts both ways).
func TestExclusiveDoesNotDisplacePassive(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	_, _, _, _ = s.Acquire(context.Background(), AttachExclusive)
	passCtx, _, _, _ := s.Acquire(context.Background(), AttachPassive)
	// Displace the exclusive.
	_, _, _, _ = s.Acquire(context.Background(), AttachExclusive)
	// Passive must still be alive.
	if passCtx.Err() != nil {
		t.Error("passive context cancelled by exclusive turnover")
	}
}

// TestReleasePassive: Release(gen) on a passive attacher must remove
// it from the passive sibling slice without touching s.clients.
func TestReleasePassive(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	_, exclGen, _, _ := s.Acquire(context.Background(), AttachExclusive)
	_, passGen, _, _ := s.Acquire(context.Background(), AttachPassive)

	s.Release(passGen)

	// Acquire another passive; the slot should be free.
	_, _, _, err := s.Acquire(context.Background(), AttachPassive)
	if err != nil {
		t.Errorf("after Release(passive), new passive failed: %v", err)
	}
	// Exclusive still alive.
	if peers := s.PeerModes(exclGen); len(peers) != 0 {
		t.Errorf("PeerModes after Release(passive) = %v, want []", peers)
	}
}

// TestClosePassiveContextsCancelled: Session.Close must cancel the
// contexts of passive attachers, not just regular ones.
func TestClosePassiveContextsCancelled(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)

	passCtx, _, _, _ := s.Acquire(context.Background(), AttachPassive)
	_ = s.Close()

	select {
	case <-passCtx.Done():
		// good
	case <-time.After(100 * time.Millisecond):
		t.Error("passive context not cancelled within 100ms of Close")
	}
}

// TestReleaseRemovesSpecificClient: Release(gen) must affect only
// the matching client; others stay attached.
func TestReleaseRemovesSpecificClient(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	_, genA, _, _ := s.Acquire(context.Background(), AttachReadonly)
	_, genB, _, _ := s.Acquire(context.Background(), AttachReadonly)
	s.Release(genA)
	// Release(genA) should leave B in place.
	if !s.IsAttached() {
		t.Error("IsAttached() false after only one Release of two readonly clients")
	}
	if peers := s.PeerModes(genB); len(peers) != 0 {
		t.Errorf("PeerModes(B) after Release(A) = %v, want []", peers)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !s.Closed() {
		t.Error("Closed() = false after Close")
	}
	// Operations on a closed session return ErrSessionClosed.
	if _, err := s.WriteStdin([]byte("x")); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("WriteStdin after Close = %v, want ErrSessionClosed", err)
	}
	if err := s.Resize(1, 1); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Resize after Close = %v, want ErrSessionClosed", err)
	}
}

func TestCloseCancelsActiveAttach(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	ctx, _, _, _ := s.Acquire(context.Background(), AttachExclusive)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		// good
	case <-time.After(time.Second):
		t.Error("Close did not cancel the active attach context within 1s")
	}
}

func TestIdleForGrows(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	time.Sleep(20 * time.Millisecond)
	if got := s.IdleFor(); got < 20*time.Millisecond {
		t.Errorf("IdleFor = %v, want ≥ 20ms", got)
	}
	// Touch resets it.
	s.Touch()
	if got := s.IdleFor(); got > 20*time.Millisecond {
		t.Errorf("IdleFor after Touch = %v, want < 20ms", got)
	}
}

// TestClearRecoverStaleGenerationDoesNotFreeSuccessor reproduces the
// v1.4.2 Codex audit follow-up race: a superseded recovery finishing
// must not clear the slot owned by its successor, or a later recovery
// can't cancel that successor. Sequence: start A, start B (cancels A),
// clear A (must be a no-op), start C (must still cancel B).
func TestClearRecoverStaleGenerationDoesNotFreeSuccessor(t *testing.T) {
	s := mustNewSession(t)

	ctxA, genA := s.TryStartRecover()
	ctxB, genB := s.TryStartRecover()

	// Starting B must cancel A.
	select {
	case <-ctxA.Done():
	default:
		t.Fatal("starting recovery B did not cancel recovery A")
	}

	// A finishes and clears with its (now stale) generation. Because B
	// owns the slot, this must be a no-op.
	s.ClearRecover(genA)

	ctxC, genC := s.TryStartRecover()

	// Starting C must cancel B — which only happens if A's stale clear
	// left B's cancel func in the slot.
	select {
	case <-ctxB.Done():
	default:
		t.Fatal("stale ClearRecover(A) freed the slot; starting C did not cancel B")
	}

	if genB == genA || genC == genB {
		t.Fatalf("generations should be monotonic and distinct: A=%d B=%d C=%d", genA, genB, genC)
	}

	// Sanity: C's context is live until C clears it.
	select {
	case <-ctxC.Done():
		t.Fatal("recovery C context cancelled unexpectedly")
	default:
	}
	s.ClearRecover(genC)
}

// --- exclusive-if-free (first-attach-wins, v1.7.0) ---

func TestAcquireExclusiveIfFreeGrantsExclusiveWhenFree(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	ctx, gen, granted, err := s.Acquire(context.Background(), AttachExclusiveIfFree)
	if err != nil {
		t.Fatal(err)
	}
	if granted != AttachExclusive {
		t.Errorf("granted = %v, want AttachExclusive", granted)
	}
	if ctx.Err() != nil {
		t.Error("context cancelled at acquire time")
	}
	// The granted role must be what PeerModes reports to others.
	_, roGen, _, err := s.Acquire(context.Background(), AttachReadonly)
	if err != nil {
		t.Fatal(err)
	}
	peers := s.PeerModes(roGen)
	if len(peers) != 1 || peers[0] != "exclusive" {
		t.Errorf("peers = %v, want [exclusive]", peers)
	}
	// After the holder releases, a fresh if-free attach gets exclusive.
	s.Release(gen)
	_, _, granted2, err := s.Acquire(context.Background(), AttachExclusiveIfFree)
	if err != nil {
		t.Fatal(err)
	}
	if granted2 != AttachExclusive {
		t.Errorf("granted after release = %v, want AttachExclusive", granted2)
	}
}

func TestAcquireExclusiveIfFreeGrantsReadonlyWhenHeld(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	holderCtx, holderGen, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	roCtx, roGen, granted, err := s.Acquire(context.Background(), AttachExclusiveIfFree)
	if err != nil {
		t.Fatal(err)
	}
	if granted != AttachReadonly {
		t.Errorf("granted = %v, want AttachReadonly", granted)
	}
	// The holder must NOT be displaced — that's the whole point.
	if holderCtx.Err() != nil {
		t.Error("exclusive holder cancelled by exclusive-if-free attach")
	}
	if roCtx.Err() != nil {
		t.Error("readonly grant cancelled at acquire time")
	}
	if peers := s.PeerModes(holderGen); len(peers) != 1 || peers[0] != "readonly" {
		t.Errorf("holder's peers = %v, want [readonly]", peers)
	}
	if peers := s.PeerModes(roGen); len(peers) != 1 || peers[0] != "exclusive" {
		t.Errorf("viewer's peers = %v, want [exclusive]", peers)
	}
}

// Same client id → an exclusive-if-free attach reclaims exclusive, silently
// displacing that client's own stale connection (the reconnect / cold-start
// self-collision fix). The displaced holder IS cancelled.
func TestAcquireExclusiveIfFreeSameClientHandsOver(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	holderCtx, _, _, err := s.AcquireClient(context.Background(), AttachExclusive, "deviceA")
	if err != nil {
		t.Fatal(err)
	}
	newCtx, _, granted, err := s.AcquireClient(context.Background(), AttachExclusiveIfFree, "deviceA")
	if err != nil {
		t.Fatal(err)
	}
	if granted != AttachExclusive {
		t.Errorf("granted = %v, want AttachExclusive (same-client handover)", granted)
	}
	if holderCtx.Err() == nil {
		t.Error("stale same-client holder was NOT displaced")
	}
	if newCtx.Err() != nil {
		t.Error("new exclusive grant cancelled at acquire time")
	}
}

// Different client id → exclusive-if-free still downgrades to readonly and the
// genuine other-device holder is preserved (the "Live on another device" path).
func TestAcquireExclusiveIfFreeDifferentClientReadonly(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	holderCtx, _, _, err := s.AcquireClient(context.Background(), AttachExclusive, "deviceA")
	if err != nil {
		t.Fatal(err)
	}
	_, _, granted, err := s.AcquireClient(context.Background(), AttachExclusiveIfFree, "deviceB")
	if err != nil {
		t.Fatal(err)
	}
	if granted != AttachReadonly {
		t.Errorf("granted = %v, want AttachReadonly (different client)", granted)
	}
	if holderCtx.Err() != nil {
		t.Error("genuine other-device holder displaced by a different client's if-free attach")
	}
}

func TestAcquireExclusiveIfFreeIgnoresReadonlyPeers(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	// Only readonly watchers attached — the session is "free".
	watcher, _, _, err := s.Acquire(context.Background(), AttachReadonly)
	if err != nil {
		t.Fatal(err)
	}
	_, _, granted, err := s.Acquire(context.Background(), AttachExclusiveIfFree)
	if err != nil {
		t.Fatal(err)
	}
	if granted != AttachExclusive {
		t.Errorf("granted = %v, want AttachExclusive (readonly peers don't hold)", granted)
	}
	if watcher.Err() != nil {
		t.Error("readonly watcher cancelled by exclusive-if-free grant")
	}
}

func TestAcquireExclusiveIfFreeRespectsReadonlyCap(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	if _, _, _, err := s.Acquire(context.Background(), AttachExclusive); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxReadonlyPerSession; i++ {
		if _, _, _, err := s.Acquire(context.Background(), AttachReadonly); err != nil {
			t.Fatalf("readonly %d: %v", i, err)
		}
	}
	// Holder present + readonly slots full → if-free resolves to
	// readonly and must hit the cap, not displace the holder.
	_, _, _, err := s.Acquire(context.Background(), AttachExclusiveIfFree)
	if err != ErrReadonlyCapacity {
		t.Errorf("err = %v, want ErrReadonlyCapacity", err)
	}
}

func TestReplacedNotifierFiresBeforeCancel(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	firstCtx, gen1, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	notified := make(chan bool, 1) // value = "ctx still alive at notify time"
	s.SetReplacedNotifier(gen1, func() {
		notified <- firstCtx.Err() == nil
	})

	if _, _, _, err := s.Acquire(context.Background(), AttachExclusive); err != nil {
		t.Fatal(err)
	}
	select {
	case alive := <-notified:
		if !alive {
			t.Error("notifyReplaced ran AFTER the displaced context was cancelled — Goodbye would race the teardown")
		}
	default:
		t.Fatal("notifyReplaced not called on displacement")
	}
	if firstCtx.Err() == nil {
		t.Error("displaced context not cancelled after notification")
	}
}

func TestSetReplacedNotifierStaleGenIsNoop(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	_, gen1, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	s.Release(gen1)
	// Stale install: must not panic, must not fire on later displacement.
	fired := false
	s.SetReplacedNotifier(gen1, func() { fired = true })

	if _, _, _, err := s.Acquire(context.Background(), AttachExclusive); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Acquire(context.Background(), AttachExclusive); err != nil {
		t.Fatal(err)
	}
	if fired {
		t.Error("stale-gen notifier fired")
	}
}

func TestReplacedNotifierRunsOutsideSessionLock(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	_, gen1, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	// A notifier that takes s.mu itself (PeerModes does). If the
	// displacement loop ever runs while Acquire still holds the
	// lock, this deadlocks — the test hangs and fails on timeout.
	called := make(chan struct{}, 1)
	s.SetReplacedNotifier(gen1, func() {
		_ = s.PeerModes(gen1)
		called <- struct{}{}
	})
	done := make(chan struct{})
	go func() {
		if _, _, _, err := s.Acquire(context.Background(), AttachExclusive); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire deadlocked — notifier ran under s.mu")
	}
	select {
	case <-called:
	default:
		t.Error("notifier not called")
	}
}

func TestReplacedNotifierPanicDoesNotBreakDisplacement(t *testing.T) {
	t.Parallel()
	id, _ := NewSessionID()
	pty := newFakePTY()
	s, _ := NewSession(id, "", pty, 24, 80, 1024, 0)
	defer s.Close()

	firstCtx, gen1, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReplacedNotifier(gen1, func() { panic("notifier exploded") })

	secondCtx, _, _, err := s.Acquire(context.Background(), AttachExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if firstCtx.Err() == nil {
		t.Error("displaced context not cancelled despite notifier panic")
	}
	if secondCtx.Err() != nil {
		t.Error("new attacher's context cancelled")
	}
}
