//go:build linux

package ptysidecar

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// waitForFgState reads frames until a FrameFgState whose body equals
// `want` arrives, or the timeout passes (returning the last value
// seen for the failure message). Other frame types (stdout echo,
// echo-state) are skipped.
func waitForFgState(t *testing.T, conn net.Conn, want string, timeout time.Duration) {
	t.Helper()
	last := "<none>"
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		ft, body, err := ReadFrame(conn)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			t.Fatalf("ReadFrame: %v", err)
		}
		if ft != FrameFgState {
			continue
		}
		last = string(body)
		if last == want {
			return
		}
	}
	t.Fatalf("no FrameFgState %q within %s (last seen %q)", want, timeout, last)
}

// TestSidecarForegroundCommTracksJobControl drives the real
// mechanism end to end: an interactive bash on the sidecar's PTY,
// `sleep` moved into the foreground, ^C back to the shell — the
// poller must report each transition. Not parallel: it shrinks the
// package-level fgPollInterval.
func TestSidecarForegroundCommTracksJobControl(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("needs /bin/bash for job control (fg process groups)")
	}
	orig := fgPollInterval
	fgPollInterval = 200 * time.Millisecond
	defer func() { fgPollInterval = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sockPath, _, wait := startSidecar(t, ctx, func(c *Config) {
		c.Shell = "/bin/bash"
		c.ShellArgs = []string{"-i"}
	})

	conn := dialWithRetry(t, sockPath)
	defer conn.Close()

	// Idle interactive shell: the shell itself owns the terminal.
	waitForFgState(t, conn, "bash", 5*time.Second)

	// Foreground job: bash gives sleep its own process group and
	// hands it the terminal.
	if err := WriteFrame(conn, FrameStdin, []byte("sleep 30\n")); err != nil {
		t.Fatalf("WriteFrame stdin: %v", err)
	}
	waitForFgState(t, conn, "sleep", 5*time.Second)

	// ^C kills the foreground job; the terminal returns to bash.
	if err := WriteFrame(conn, FrameStdin, []byte{0x03}); err != nil {
		t.Fatalf("WriteFrame ^C: %v", err)
	}
	waitForFgState(t, conn, "bash", 5*time.Second)

	cancel()
	_ = wait()
}

// capComm must always yield valid UTF-8 ≤ MaxFgCommBytes — kernel
// task names are arbitrary bytes, and an invalid-UTF-8 CBOR text
// string would fail the protocol marshal.
func TestCapCommSanitizesUTF8(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude", "claude"},
		{"bad\xff\xfename", "badname"},
		{string(make([]byte, 40)), string(make([]byte, MaxFgCommBytes))}, // NULs are valid UTF-8
	}
	for _, tc := range cases {
		if got := capComm(tc.in); got != tc.want {
			t.Errorf("capComm(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Rune split at the byte cap: 31 ASCII bytes + a 3-byte rune →
	// the torn rune is dropped, output stays valid UTF-8.
	in := strings.Repeat("a", MaxFgCommBytes-1) + "€"
	got := capComm(in)
	if len(got) > MaxFgCommBytes || !utf8.ValidString(got) {
		t.Errorf("capComm rune-split: got %q (len %d, valid=%v)", got, len(got), utf8.ValidString(got))
	}
}
