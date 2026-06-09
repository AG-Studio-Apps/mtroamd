package ptyclient

import (
	"testing"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/ptysidecar"
)

// TestConnCachesForegroundCommAndCwd verifies the Conn caches the
// foreground command (FrameFgState) and its cwd (FrameFgCwd) from the
// sidecar's poller. The transition anchors (fg_since / fg_seq) are
// NOT conn's responsibility — the session derives them in ring space.
func TestConnCachesForegroundCommAndCwd(t *testing.T) {
	conn, sidecar := pipePair(t)

	go func() {
		_ = ptysidecar.WriteFrame(sidecar, ptysidecar.FrameFgState, []byte("claude"))
		_ = ptysidecar.WriteFrame(sidecar, ptysidecar.FrameFgCwd, []byte("/home/u/proj"))
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn.ForegroundComm() == "claude" && conn.ForegroundCwd() == "/home/u/proj" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := conn.ForegroundComm(); got != "claude" {
		t.Errorf("ForegroundComm = %q, want claude", got)
	}
	if got := conn.ForegroundCwd(); got != "/home/u/proj" {
		t.Errorf("ForegroundCwd = %q, want /home/u/proj", got)
	}
}

// TestConnSanitizesForegroundBytes verifies invalid-UTF-8 / oversized
// fg and cwd bodies are coerced so they can't fail a downstream CBOR
// marshal (SanitizeCapped). A torn rune at the cap boundary is dropped.
func TestConnSanitizesForegroundBytes(t *testing.T) {
	conn, sidecar := pipePair(t)

	// Invalid UTF-8 comm + an over-cap cwd.
	badComm := []byte{0xff, 0xfe, 'v', 'i', 'm'}
	bigCwd := make([]byte, ptysidecar.MaxFgCwdBytes+50)
	for i := range bigCwd {
		bigCwd[i] = 'a'
	}
	go func() {
		_ = ptysidecar.WriteFrame(sidecar, ptysidecar.FrameFgState, badComm)
		_ = ptysidecar.WriteFrame(sidecar, ptysidecar.FrameFgCwd, bigCwd)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn.ForegroundCwd() != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := conn.ForegroundComm(); got != "vim" {
		t.Errorf("ForegroundComm = %q, want vim (invalid leading bytes stripped)", got)
	}
	if got := conn.ForegroundCwd(); len(got) > ptysidecar.MaxFgCwdBytes {
		t.Errorf("ForegroundCwd len = %d, want ≤ %d", len(got), ptysidecar.MaxFgCwdBytes)
	}
}
