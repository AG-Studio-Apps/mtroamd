package session

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// nullLogger discards everything. We don't want test output flooded
// with the "load_dropped" warnings the negative-path tests deliberately
// trigger.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makePersistedSession spawns a fresh Session, marks it persistent,
// writes some test scrollback into the buffer, and returns it. The
// caller is responsible for cleanup via the temp-dir t.TempDir.
func makePersistedSession(t *testing.T, payload []byte) *Session {
	t.Helper()
	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	s, err := NewSession(id, "persist-test", newFakePTY(), 24, 80, 1024, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.SetPersist(true)
	if len(payload) > 0 {
		_, _ = s.buf.Write(payload)
	}
	return s
}

// TestSaveToAndLoadPersistedRoundTrip is the load-bearing positive
// test: spawn → save → load (into a fresh registry) → confirm the
// hydrated session reports the same state and replays the same bytes.
func TestSaveToAndLoadPersistedRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	payload := []byte("hello, persisted world\nthis is the scrollback the user should see on restore\n")

	original := makePersistedSession(t, payload)
	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	// On-disk shape sanity check.
	sessionDir := filepath.Join(dir, sessionsSubdir, original.ID().String())
	for _, f := range []string{metaFilename, scrollbackFilename} {
		info, err := os.Stat(filepath.Join(sessionDir, f))
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 0600", f, info.Mode().Perm())
		}
	}

	// Fresh registry — simulates daemon restart.
	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	n, err := LoadPersisted(dir, reg, nullLogger())
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if n != 1 {
		t.Fatalf("LoadPersisted count = %d, want 1", n)
	}

	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("registry.Lookup: %v", err)
	}
	if got := restored.Name(); got != original.Name() {
		t.Errorf("Name = %q, want %q", got, original.Name())
	}
	if !restored.RestoredFromDisk() {
		t.Error("restored session should have RestoredFromDisk() == true")
	}
	if !restored.Persist() {
		t.Error("persist flag did not round-trip")
	}
	rows, cols := restored.WindowSize()
	if rows != 24 || cols != 80 {
		t.Errorf("WindowSize = %d×%d, want 24×80", rows, cols)
	}

	// Most important: replay the scrollback.
	data, _, _ := restored.Buffer().ReadSince(0, 0)
	if string(data) != string(payload) {
		t.Errorf("scrollback mismatch:\n  got:  %q\n  want: %q", data, payload)
	}
}

// TestWedgeWatcherInitialisedOnRestore pins a regression: an early
// version of LoadPersisted constructed Session via a direct struct
// literal that omitted the `wedge` field. Restored sessions ended up
// with `wedge == nil`, and every nil-guarded call site (Resize →
// ArmResize, Pump → ObserveBytes, OnWedge subscriber install)
// silently no-opped. The result: any session that survived a daemon
// restart lost detection for the rest of its lifetime — visible to
// operators only as "session-info shows zero resizes despite many
// resize control frames in journalctl."
func TestWedgeWatcherInitialisedOnRestore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	original := makePersistedSession(t, []byte("y"))
	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// Restored sessions have lazy PTY (pty == nil until first
	// attach), so calling Resize from a test panics on nil
	// pty.SetSize. Drop into package-internal field access for the
	// proof: the watcher must be a live struct, not nil. Behaviour
	// is then exercised by feeding bytes directly — that path is
	// PTY-independent, increments totalOutBytes, and proves the
	// watcher is functional end-to-end.
	if restored.wedge == nil {
		t.Fatal("restored session has nil wedge watcher — " +
			"regression: loadSessionFromDir must initialise it")
	}
	restored.wedge.ObserveBytes([]byte("xxxx"), restored.Created())
	total, _, _, _, _ := restored.WedgeSnapshot()
	if total != 4 {
		t.Fatalf("watcher should count bytes from ObserveBytes; got %d, want 4", total)
	}
}

// TestLastSidecarSeqRoundTrip checks that Session.lastSidecarSeq is
// persisted to meta.cbor and restored across LoadPersisted — the
// daemon needs this watermark to send FrameResume(from_seq) on
// reattach without re-consuming bytes already in the daemon ring.
func TestLastSidecarSeqRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	original := makePersistedSession(t, []byte("x"))
	original.AdvanceSidecarSeq(987654321)
	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := restored.LastSidecarSeq(); got != 987654321 {
		t.Errorf("LastSidecarSeq round-trip: want 987654321, got %d", got)
	}
}

// TestAltScreenActiveRoundTrip pins the v1.1.2 fix for bug A: a
// session that was on the alternate screen at save time must come
// back with the wedge watcher's alt-screen tracker re-armed across a
// daemon restart. Without persistence, the original DECSET 1049h
// sits in the scrollback ring rather than the live PTY stream, the
// watcher never observes it post-attach, and the vertical_walk gate
// stays silenced for the rest of the session.
func TestAltScreenActiveRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	original := makePersistedSession(t, []byte("z"))
	// Drive the tracker into the active state by feeding a real
	// DECSET 1049h. Avoids reaching into private fields.
	original.wedge.ObserveBytes([]byte("\x1b[?1049h"), original.Created())
	if !original.wedge.AltScreenActive() {
		t.Fatal("precondition: tracker should be active after DECSET 1049h")
	}
	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !restored.wedge.AltScreenActive() {
		t.Fatal("alt-screen state lost across SaveTo/LoadPersisted — " +
			"bug A regression")
	}
}

// TestAdvanceSidecarSeqMonotonic verifies that AdvanceSidecarSeq is
// a monotonic watermark — older values do not regress lastSidecarSeq.
// The Pump's coalesced-ack flow can otherwise advance the watermark
// out of order if a stale frame slips through.
func TestAdvanceSidecarSeqMonotonic(t *testing.T) {
	t.Parallel()
	s := makePersistedSession(t, nil)
	s.AdvanceSidecarSeq(100)
	s.AdvanceSidecarSeq(50)
	if got := s.LastSidecarSeq(); got != 100 {
		t.Errorf("monotonic: regressed to %d, want 100", got)
	}
	s.AdvanceSidecarSeq(200)
	if got := s.LastSidecarSeq(); got != 200 {
		t.Errorf("advance: want 200, got %d", got)
	}
}

// TestSaveToNoOpWhenNotPersisting verifies that calling SaveTo on a
// session whose persist flag is false is harmless — no dir created.
// The flusher relies on this guard so callers don't have to.
func TestSaveToNoOpWhenNotPersisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id, _ := NewSessionID()
	s, err := NewSession(id, "ephemeral", newFakePTY(), 24, 80, 1024, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// SetPersist not called — defaults to false.
	if err := s.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo on non-persisting session: %v", err)
	}
	sessionDir := filepath.Join(dir, sessionsSubdir, s.ID().String())
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("session dir created despite persist=false: %v", err)
	}
}

// TestLoadPersistedDropsCorruptMeta verifies the "log and skip"
// posture: a corrupted meta.cbor in one session shouldn't crash the
// daemon or block sibling sessions from loading.
func TestLoadPersistedDropsCorruptMeta(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	good := makePersistedSession(t, []byte("clean session"))
	if err := good.SaveTo(dir); err != nil {
		t.Fatalf("save good: %v", err)
	}

	// Plant a corrupted-meta session in a sibling directory.
	badDir := filepath.Join(dir, sessionsSubdir, "ffffffffffffffffffffffffffffffff")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, metaFilename),
		[]byte("not valid cbor"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, scrollbackFilename),
		make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	n, err := LoadPersisted(dir, reg, nullLogger())
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if n != 1 {
		t.Errorf("loaded %d sessions, want 1 (the good one)", n)
	}
	// Corrupt dir should be removed.
	if _, err := os.Stat(badDir); !os.IsNotExist(err) {
		t.Errorf("corrupt session dir wasn't cleaned up")
	}
}

// TestLoadPersistedDropsFormatVersionMismatch verifies that a meta
// from a future / past schema is dropped without breaking the load.
// Protects against "daemon downgraded after running a newer version
// and leaving incompatible files on disk."
func TestLoadPersistedDropsFormatVersionMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s := makePersistedSession(t, []byte("hi"))
	if err := s.SaveTo(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Manually rewrite meta with a bumped FormatVersion.
	sessionDir := filepath.Join(dir, sessionsSubdir, s.ID().String())
	metaPath := filepath.Join(sessionDir, metaFilename)
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	// Hack: flip the fv field's value byte. We know it's there;
	// rewriting via CBOR would be cleaner but this is sufficient
	// to break the version check.
	_ = metaBytes
	// Easier path: replace the whole meta with an explicit bad version.
	if err := os.WriteFile(metaPath, badFormatMeta(t, s, 9999), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	n, _ := LoadPersisted(dir, reg, nullLogger())
	if n != 0 {
		t.Errorf("loaded %d sessions, want 0 (version mismatch)", n)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("mismatched-version dir wasn't cleaned up")
	}
}

// TestLoadPersistedDropsExpiredOnLoad: a session whose idleTimeout
// has elapsed by the time the daemon restarts is dropped without
// being added to the registry. Prevents zombie sessions from
// hanging around forever just because they were persisted.
func TestLoadPersistedDropsExpiredOnLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	id, _ := NewSessionID()
	s, err := NewSession(id, "stale", newFakePTY(), 24, 80, 1024, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPersist(true)
	_, _ = s.buf.Write([]byte("old"))
	if err := s.SaveTo(dir); err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond)

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	n, err := LoadPersisted(dir, reg, nullLogger())
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if n != 0 {
		t.Errorf("loaded %d sessions, want 0 (expired)", n)
	}
}

// TestDeletePersistedRemovesDirectory: GC + Kill path removes the
// on-disk dir so reaped sessions don't leak.
func TestDeletePersistedRemovesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s := makePersistedSession(t, []byte("delete me"))
	if err := s.SaveTo(dir); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(dir, sessionsSubdir, s.ID().String())
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("pre-condition: dir should exist: %v", err)
	}

	if err := s.DeletePersisted(dir); err != nil {
		t.Fatalf("DeletePersisted: %v", err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("DeletePersisted did not remove the dir")
	}

	// Second call is a no-op (idempotent).
	if err := s.DeletePersisted(dir); err != nil {
		t.Errorf("second DeletePersisted should be a no-op: %v", err)
	}
}

// TestStartFlusherWritesOnInterval: the background goroutine fires
// on its ticker cadence, advances lastSnapshotSeq, and produces a
// readable on-disk snapshot. Uses a short interval (50ms) so the
// test wraps quickly.
func TestStartFlusherWritesOnInterval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := makePersistedSession(t, []byte("initial"))

	// Pre-flusher: no on-disk state yet.
	sessionDir := filepath.Join(dir, sessionsSubdir, s.ID().String())
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("pre-flusher state: dir should not exist: %v", err)
	}

	s.StartFlusher(dir, 50*time.Millisecond, nullLogger())
	t.Cleanup(func() { _ = s.Close() }) // also stops the flusher

	// Wait for at least one tick — flusher needs to fire and call
	// SaveTo. 200ms gives plenty of margin.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(sessionDir, metaFilename)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, metaFilename)); err != nil {
		t.Fatalf("flusher did not write meta.cbor within 500ms: %v", err)
	}

	// Push more bytes; expect the next tick to update the file.
	_, _ = s.buf.Write([]byte("\nmore output"))
	time.Sleep(120 * time.Millisecond)

	// Verify the on-disk scrollback reflects the new content via a
	// fresh Load.
	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(s.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	data, _, _ := restored.Buffer().ReadSince(0, 0)
	if want := "initial\nmore output"; string(data) != want {
		t.Errorf("restored buffer = %q, want %q", data, want)
	}
}

// TestFlusherFinalFlushOnClose: the ctx-done path inside the flusher
// performs one final SaveTo before exiting, so a dirty session that
// hadn't yet reached its next interval is still preserved on
// daemon shutdown.
func TestFlusherFinalFlushOnClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := makePersistedSession(t, []byte("before-close"))

	// Long interval so the ticker never fires during the test —
	// the only write we expect is the final flush from stopFlusher.
	s.StartFlusher(dir, 1*time.Hour, nullLogger())

	// Append more after the flusher started; this is what the final
	// flush should capture.
	_, _ = s.buf.Write([]byte("-final"))

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(s.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	data, _, _ := restored.Buffer().ReadSince(0, 0)
	if want := "before-close-final"; string(data) != want {
		t.Errorf("restored buffer after final flush = %q, want %q", data, want)
	}
}

// TestResolvePersistTriState verifies the Registry's nil/true/false
// resolution. nil → daemon default (default-on), true/false → as-is.
func TestResolvePersistTriState(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	cases := []struct {
		name      string
		def       bool
		requested *bool
		want      bool
	}{
		{"nil with default on", true, nil, true},
		{"nil with default off", false, nil, false},
		{"explicit true overrides default off", false, &yes, true},
		{"explicit false overrides default on", true, &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(0, time.Hour, time.Hour, 0)
			r.SetPersistenceDefault(tc.def)
			if got := r.ResolvePersist(tc.requested); got != tc.want {
				t.Errorf("ResolvePersist(%v) with default=%v = %v, want %v",
					tc.requested, tc.def, got, tc.want)
			}
		})
	}
}

// TestRemoveDeletesOnDiskState: explicit Remove (mtroam kill path)
// drops the on-disk session dir so reaped sessions don't leak disk.
func TestRemoveDeletesOnDiskState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := makePersistedSession(t, []byte("kill me"))
	if err := s.SaveTo(dir); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	reg.SetStateDir(dir)
	if err := reg.Add(s); err != nil {
		t.Fatal(err)
	}

	sessionDir := filepath.Join(dir, sessionsSubdir, s.ID().String())
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("pre-condition: dir should exist: %v", err)
	}

	reg.Remove(s.ID())

	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("Remove did not delete on-disk state")
	}
}

// TestLoadPersistedReturnsZeroOnMissingDir: a fresh daemon install
// (no sessions/ dir yet) should not error.
func TestLoadPersistedReturnsZeroOnMissingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	n, err := LoadPersisted(dir, reg, nullLogger())
	if err != nil {
		t.Fatalf("LoadPersisted on empty parent: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// badFormatMeta returns a meta.cbor with a bumped format version,
// used by the version-mismatch test. Other fields copied from the
// source session so the file is syntactically valid CBOR.
func badFormatMeta(t *testing.T, s *Session, version int) []byte {
	t.Helper()
	bufBytes, writePos, headSeq, full := s.buf.Snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := persistedSessionMeta{
		FormatVersion: version,
		SessionID:     append([]byte(nil), s.id[:]...),
		Name:          s.name,
		CreatedNs:     s.created.UnixNano(),
		LastActiveNs:  s.lastActiveAt.UnixNano(),
		Rows:          s.rows,
		Cols:          s.cols,
		IdleTimeoutNs: int64(s.idleTimeout),
		Persist:       s.persist,
		BufCapacity:   len(bufBytes),
		HeadSeq:       headSeq,
		WritePos:      writePos,
		Full:          full,
	}
	out, err := cbor.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// metaWithBufCapacity returns a meta.cbor whose BufCapacity field is
// set to the supplied value. Used to verify the pre-v1.0 cap rejects
// crafted entries before NewRingBuffer attempts a huge allocation.
func metaWithBufCapacity(t *testing.T, s *Session, capacity int) []byte {
	t.Helper()
	_, writePos, headSeq, full := s.buf.Snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := persistedSessionMeta{
		FormatVersion: persistenceFormatVersion,
		SessionID:     append([]byte(nil), s.id[:]...),
		Name:          s.name,
		CreatedNs:     s.created.UnixNano(),
		LastActiveNs:  s.lastActiveAt.UnixNano(),
		Rows:          s.rows,
		Cols:          s.cols,
		IdleTimeoutNs: int64(s.idleTimeout),
		Persist:       s.persist,
		BufCapacity:   capacity,
		HeadSeq:       headSeq,
		WritePos:      writePos,
		Full:          full,
	}
	out, err := cbor.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLoadPersistedDropsOversizedBufCapacity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s := makePersistedSession(t, []byte("hi"))
	if err := s.SaveTo(dir); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Rewrite meta with a BufCapacity over the cap. Defends against a
	// crafted meta.cbor that would crash daemon startup at the
	// `make([]byte, meta.BufCapacity)` inside NewRingBuffer.
	sessionDir := filepath.Join(dir, sessionsSubdir, s.ID().String())
	metaPath := filepath.Join(sessionDir, metaFilename)
	if err := os.WriteFile(metaPath,
		metaWithBufCapacity(t, s, maxPersistedBufCapacity+1), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	n, _ := LoadPersisted(dir, reg, nullLogger())
	if n != 0 {
		t.Errorf("loaded %d sessions, want 0 (BufCapacity over cap)", n)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("oversized-BufCapacity dir wasn't cleaned up")
	}
}

// TestScreenRepaintRoundTrip is grid-persistence: a session that was on a
// faithful ALT screen at save time must come back with a faithful alt model
// (not the empty main-buffer fallback), so InjectAltScreenRepaint fires on the
// first reattach after a daemon restart instead of the raw-tail spill.
func TestScreenRepaintRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	original := makePersistedSession(t, nil)
	// Drive the live model onto the alt screen with content (as a full-screen
	// TUI would) → faithful + alt, so SaveTo persists the Repaint frame.
	original.screen.Feed([]byte("\x1b[?1049h\x1b[H\x1b[2J\x1b[2;3Hhello world\x1b[6;1Hfooter"))
	if !original.screen.AltActive() || !original.screen.Faithful() {
		t.Fatalf("setup: model not faithful alt (alt=%v faithful=%v)",
			original.screen.AltActive(), original.screen.Faithful())
	}
	want := string(original.screen.Repaint())

	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	has, alt, faithful := restored.ScreenState()
	if !has || !alt || !faithful {
		t.Fatalf("restored ScreenState has=%v alt=%v faithful=%v, want all true", has, alt, faithful)
	}
	if got := string(restored.screen.Repaint()); got != want {
		t.Fatalf("restored grid differs from original:\n  got:  %q\n  want: %q", got, want)
	}
	if _, ok := restored.InjectAltScreenRepaint(); !ok {
		t.Fatal("InjectAltScreenRepaint should return ok=true on the restored alt session")
	}
}

// TestScreenRepaintNotPersistedForMainScreen: a session NOT on the alt screen
// (or unfaithful) persists no Repaint, so it restores to a fresh model and
// falls back to raw replay — the unchanged, pre-grid-persistence behavior and
// the back-compat path for old snapshots that have no screen_repaint field.
func TestScreenRepaintNotPersistedForMainScreen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	original := makePersistedSession(t, []byte("just a shell prompt\n"))
	if original.screen.AltActive() {
		t.Fatal("setup: fresh session should be on the main screen")
	}
	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	_, alt, _ := restored.ScreenState()
	if alt {
		t.Fatal("main-screen session should not restore as alt-active")
	}
	if _, ok := restored.InjectAltScreenRepaint(); ok {
		t.Fatal("InjectAltScreenRepaint should be false for a restored main-screen session")
	}
}

// TestScreenRepaintNotPersistedWhenResizeDirty guards the resize-dirty spill:
// SaveTo must NOT persist a Repaint when the model is resize-dirty (geometry
// changed, app hasn't repainted). Restore cannot re-derive the flag (Feed clears
// it), so persisting a misplaced grid would ship the stranded-footer frame on the
// first reattach after a restart — the very spill this feature prevents.
func TestScreenRepaintNotPersistedWhenResizeDirty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	original := makePersistedSession(t, nil)
	original.screen.Feed([]byte("\x1b[?1049h\x1b[H\x1b[2J\x1b[2;3Hhello\x1b[6;1Hfooter"))
	if !original.screen.AltActive() || !original.screen.Faithful() {
		t.Fatalf("setup: model not faithful alt (alt=%v faithful=%v)",
			original.screen.AltActive(), original.screen.Faithful())
	}
	// A geometry change after the last repaint top-anchors the grid → resize-dirty.
	r, c := original.screen.Size()
	original.screen.Resize(r+4, c)
	if !original.screen.ResizeDirty() {
		t.Fatal("setup: model should be resize-dirty after a geometry change")
	}

	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	restored, err := reg.Lookup(original.ID())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := restored.InjectAltScreenRepaint(); ok {
		t.Fatal("a resize-dirty model must not persist a Repaint (would resurrect the spill on restore)")
	}
}

// TestRestoreRejectsOversizedDims guards the OOM: a corrupt/hostile meta.cbor with
// an out-of-range dimension must be DROPPED, not fed to altscreen.New (which would
// eagerly allocate a rows*cols grid and OOM-crash the daemon at startup).
func TestRestoreRejectsOversizedDims(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	original := makePersistedSession(t, []byte("hi\n"))
	if err := original.SaveTo(dir); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	// Corrupt the on-disk meta with an out-of-range dimension.
	metaPath := filepath.Join(dir, sessionsSubdir, original.ID().String(), "meta.cbor")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var m persistedSessionMeta
	if err := cbor.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Rows, m.Cols = 65535, 65535
	tampered, err := cbor.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(0, time.Hour, time.Hour, 0)
	// LoadPersisted logs + drops the bad session; it must NOT panic/OOM or fail.
	if _, err := LoadPersisted(dir, reg, nullLogger()); err != nil {
		t.Fatalf("LoadPersisted should tolerate a corrupt session (drop it), got: %v", err)
	}
	if _, err := reg.Lookup(original.ID()); err == nil {
		t.Fatal("session with oversized dims should have been dropped, not loaded")
	}
}

// TestAltScreenForClientTrustsFaithfulAltModel is the guard in the OTHER
// direction, which is the dangerous one: under-reporting alt means the client
// never primes its emulator onto the alt buffer and a truncated replay leaves it
// rendering a full-screen TUI onto the main buffer (the prompt-spill class the
// CRITICAL banner in MTRoamTransport.primeAltScreenIfNeeded guards).
func TestAltScreenForClientTrustsFaithfulAltModel(t *testing.T) {
	t.Parallel()
	s := makePersistedSession(t, []byte("z"))
	// Put the MODEL on the alt screen, and leave the tracker inactive — the
	// inverse latch (a restart that missed the ?1049h).
	s.screen.Feed([]byte("\x1b[?1049h"))
	if _, alt, faithful, _ := s.ScreenSnapshot(); !alt || !faithful {
		t.Fatalf("precondition: want a faithful alt model, got alt=%v faithful=%v",
			alt, faithful)
	}
	if !s.AltScreenForClient() {
		t.Error("AltScreenForClient refused a faithful alt-screen model — " +
			"clients will not prime the alt buffer and replay will spill")
	}
}

// TestAltScreenForClientDoesNotTrustAnIgnorantModel pins the follow-up to the
// latched-tracker fix. The first cut trusted the model whenever it was
// `has && faithful`, which was wrong in the direction that matters most.
//
// persistence.go saves a Repaint ONLY for a faithful, non-resize-dirty ALT
// model, so a session that was genuinely on the alt screen but resize-dirty (or
// unfaithful) at save time persists alt_active=true with NO Repaint.
// loadSessionFromDir then builds a fresh main-buffer model — which reports
// faithful=true because altscreen.New sets it — and feeds nothing into it. The
// model is faithful but IGNORANT, and believing it would tell the client not to
// prime its alt buffer, spilling a full-screen TUI onto the main screen.
func TestAltScreenForClientDoesNotTrustAnIgnorantModel(t *testing.T) {
	t.Parallel()
	s := makePersistedSession(t, []byte("z"))
	// Exactly the restore shape: tracker seeded true, model fresh and untouched.
	s.wedge.SetAltScreenActive(true)
	has, alt, faithful, knows := s.screenAltEvidence()
	if !has || alt || !faithful || knows {
		t.Fatalf("precondition: want a fresh faithful main model that knows "+
			"nothing, got has=%v alt=%v faithful=%v knows=%v",
			has, alt, faithful, knows)
	}
	// Foreground is unknown in this harness (no ForegroundReporter), and unknown
	// is deliberately not a shell — so the tracker must still stand.
	if !s.AltScreenForClient() {
		t.Error("AltScreenForClient believed a model that has never observed a " +
			"buffer switch — the client will not prime and replay will spill")
	}
}

// TestAltScreenForClientTrustsAnInformedMainModel is the counterpart: once the
// model has actually OBSERVED the pty return to the main buffer, its negative
// answer is authoritative and must beat a latched tracker. This is the
// "agnticStudio" case once the model has seen a real ?1049l.
func TestAltScreenForClientTrustsAnInformedMainModel(t *testing.T) {
	t.Parallel()
	s := makePersistedSession(t, []byte("z"))
	s.wedge.SetAltScreenActive(true)
	// Observe a real alt enter/exit pair, so the model KNOWS it is on main.
	s.screen.Feed([]byte("\x1b[?1049h"))
	s.screen.Feed([]byte("\x1b[?1049l"))
	if _, alt, faithful, knows := s.screenAltEvidence(); alt || !faithful || !knows {
		t.Fatalf("precondition: want an informed faithful main model, got "+
			"alt=%v faithful=%v knows=%v", alt, faithful, knows)
	}
	if s.AltScreenForClient() {
		t.Error("AltScreenForClient trusted a latched tracker over a model that " +
			"observed the pty return to the main buffer")
	}
}

// fgPTY wraps a fake PTY with a fixed foreground command so the
// ForegroundReporter path is exercised. Only the two interface methods matter;
// everything else delegates.
type fgPTY struct {
	PTY
	comm string
}

func (f fgPTY) ForegroundComm() string { return f.comm }
func (f fgPTY) ForegroundCwd() string  { return "" }

// TestAltScreenForClientShellForegroundVetoesLatchedTracker covers the ACTUAL
// shape of the reported bug, which the model alone does not resolve.
//
// Session "agnticStudio" restored with a latched alt_active=true and NO
// persisted Repaint, so its model is a fresh main-buffer grid that has never
// observed a buffer switch — ignorant, not authoritative. What makes the daemon
// stop lying to the client there is the foreground: a bash foreground cannot be
// drawing a full-screen TUI, so it vetoes a flag nothing else corroborates.
// Verified against the live daemon, which reports fg=bash for that session and
// fg=claude for a genuine alt one.
func TestAltScreenForClientShellForegroundVetoesLatchedTracker(t *testing.T) {
	t.Parallel()
	for _, comm := range []string{"bash", "zsh", "fish", "sh"} {
		t.Run(comm, func(t *testing.T) {
			s := makePersistedSession(t, []byte("z"))
			s.mu.Lock()
			s.pty = fgPTY{PTY: s.pty, comm: comm}
			s.mu.Unlock()
			s.wedge.SetAltScreenActive(true)
			if _, _, _, knows := s.screenAltEvidence(); knows {
				t.Fatal("precondition: model should know nothing about the buffer")
			}
			if s.AltScreenForClient() {
				t.Errorf("a %s foreground did not veto a latched tracker — the "+
					"client will prime its alt buffer into a shell and pans die", comm)
			}
		})
	}
}

// TestAltScreenForClientTuiForegroundKeepsLatchedTracker is the other half: the
// veto must be narrow. With the same ignorant model, a TUI foreground means the
// latched flag is probably RIGHT, and reporting false would stop the client
// priming and spill the TUI onto the main buffer.
func TestAltScreenForClientTuiForegroundKeepsLatchedTracker(t *testing.T) {
	t.Parallel()
	for _, comm := range []string{"claude", "vim", "htop", ""} {
		t.Run("comm="+comm, func(t *testing.T) {
			s := makePersistedSession(t, []byte("z"))
			s.mu.Lock()
			s.pty = fgPTY{PTY: s.pty, comm: comm}
			s.mu.Unlock()
			s.wedge.SetAltScreenActive(true)
			if !s.AltScreenForClient() {
				t.Errorf("foreground %q wrongly vetoed the tracker — replay will "+
					"spill a full-screen TUI onto the main buffer", comm)
			}
		})
	}
}
