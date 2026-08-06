package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReadAndDeleteEnvFile covers the connect-side consumer of an SFTP-staged
// env file: it parses KEY=VAL lines (skipping blanks/comments, tolerating '='
// in values, dropping malformed lines) and ALWAYS deletes the file so a staged
// secret never lingers on the host.
func TestReadAndDeleteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	content := "# comment\nFOO=bar\n\nTOKEN=abc=def==\nNOEQUALS\n=noKey\nBAZ=qux\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := readAndDeleteEnvFile(path)
	if err != nil {
		t.Fatalf("readAndDeleteEnvFile: %v", err)
	}
	want := map[string]string{"FOO": "bar", "TOKEN": "abc=def==", "BAZ": "qux"}
	if len(env) != len(want) {
		t.Fatalf("parsed %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
	// File must be gone (no lingering plaintext secret).
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("env file still present after read: err=%v", err)
	}
}

// TestReadAndDeleteEnvFileMissingStillCleans: a missing file returns an error
// but the delete attempt is harmless (nothing to leak).
func TestReadAndDeleteEnvFileMissing(t *testing.T) {
	_, err := readAndDeleteEnvFile(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// TestWaitForSocket covers the poll loop the connect-time auto-start uses to
// decide the daemon came back up: it must return true once a socket starts
// accepting, and false when nothing ever listens within the budget.
func TestWaitForSocket(t *testing.T) {
	t.Run("appears after a delay", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "d.sock")
		go func() {
			time.Sleep(150 * time.Millisecond)
			ln, err := net.Listen("unix", path)
			if err != nil {
				return
			}
			// Keep it open for the duration of the test.
			t.Cleanup(func() { _ = ln.Close() })
		}()
		if !waitForSocket(context.Background(), path, 3*time.Second) {
			t.Fatal("expected waitForSocket to see the socket come up")
		}
	})

	t.Run("never appears", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.sock")
		if waitForSocket(context.Background(), path, 300*time.Millisecond) {
			t.Fatal("expected waitForSocket to time out on a missing socket")
		}
	})

	t.Run("context cancelled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.sock")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if waitForSocket(ctx, path, 5*time.Second) {
			t.Fatal("expected waitForSocket to bail on a cancelled context")
		}
	})
}

// --- bridgeStdio teardown ---------------------------------------------------
//
// These cover the leak found on 2026-08-06, when a box froze after ~275
// `mtroamd connect` processes accumulated over a 31-day uptime. The daemon was
// innocent: the journal showed 2770 attaches and 2770 detaches, perfectly
// balanced. It closed every client; the clients just never noticed.

// stallGrace is the deadline these tests give the bridge to notice a teardown.
// Generous relative to the real thing (which should be instant) so a loaded CI
// box cannot flake it, but far below any "hung forever" reading.
const stallGrace = 10 * time.Second

// runBridge runs bridgeStdio and reports whether it returned in time.
func runBridge(conn net.Conn, in io.Reader, out io.Writer) bool {
	return runBridgeWithStall(conn, in, out, stdoutStallTimeout)
}

func runBridgeWithStall(conn net.Conn, in io.Reader, out io.Writer, stall time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridgeStdioWithStall(conn, in, out, stall)
	}()
	select {
	case <-done:
		return true
	case <-time.After(stallGrace):
		return false
	}
}

// TestBridgeExitsWhenDaemonClosesWithWedgedStdout is THE regression test.
//
// It reproduces the real shape: the SSH channel's stdout pipe has filled
// because the far side stopped draining (phone dropped off, app killed), so the
// daemon→stdout writer is pinned inside write(2). Meanwhile stdin never hits
// EOF, because tailscaled is still holding the pipe open. That combination is
// what left ~275 of these processes alive on the box.
//
// It uses a real loopback socket rather than net.Pipe: net.Pipe is synchronous
// and unbuffered, so nothing would ever accumulate in the stdout pipe and the
// wedge could not be staged at all. (An earlier version of this test made both
// mistakes and passed against the BUGGY code, which is worse than no test.)
func TestBridgeExitsWhenDaemonClosesWithWedgedStdout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	served := make(chan struct{})
	go func() {
		defer close(served)
		srv, err := ln.Accept()
		if err != nil {
			return
		}
		// Far more than the pipe buffer (64 KiB) plus the bridge's queue, so
		// the writer is definitely wedged with the reader backed up behind it.
		payload := make([]byte, 4*1024*1024)
		_, _ = srv.Write(payload)
		time.Sleep(300 * time.Millisecond)
		srv.Close() // the daemon releases the client
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// A pipe nobody drains: writes block once the buffer fills.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	// stdin stays open: EOF here is the one exit path that already worked, so
	// withholding it is what makes this test meaningful.
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()

	if !runBridgeWithStall(conn, stdin, pw, 750*time.Millisecond) {
		t.Fatal("bridge did not exit with stdout wedged — this is the process leak " +
			"that froze the box on 2026-08-06")
	}
	<-served
}

// TestBridgeExitsOnStdinEOF guards the path that already worked: tailscaled
// closes the channel, stdin hits EOF, the bridge tears down.
func TestBridgeExitsOnStdinEOF(t *testing.T) {
	client, daemon := net.Pipe()
	defer client.Close()
	defer daemon.Close()

	// Drain whatever the daemon side sends so the writer never wedges.
	go func() { _, _ = io.Copy(io.Discard, daemon) }()

	if !runBridge(client, strings.NewReader(""), io.Discard) {
		t.Fatal("bridge did not exit on stdin EOF")
	}
}

// TestBridgeExitsWhenDaemonClosesCleanly is the ordinary case: stdout is being
// drained normally and the daemon hangs up.
func TestBridgeExitsWhenDaemonClosesCleanly(t *testing.T) {
	client, daemon := net.Pipe()
	defer client.Close()

	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()

	go func() {
		_, _ = daemon.Write([]byte("hello"))
		daemon.Close()
	}()

	if !runBridge(client, stdin, io.Discard) {
		t.Fatal("bridge did not exit when the daemon closed")
	}
}

// TestBridgeForwardsStdinIntact checks the stdin→daemon direction is a faithful
// byte tunnel: the bridge carries the mtRoam wire protocol, so reordering or
// dropping a chunk would be worse than the leak this change fixes.
func TestBridgeForwardsStdinIntact(t *testing.T) {
	client, daemon := net.Pipe()
	defer client.Close()

	want := make([]byte, 128*1024)
	for i := range want {
		want[i] = byte(i % 251) // prime stride: catches chunk-boundary reordering
	}

	var got bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.CopyN(&got, daemon, int64(len(want)))
		daemon.Close()
	}()

	if !runBridge(client, bytes.NewReader(want), io.Discard) {
		t.Fatal("bridge did not exit after stdin EOF")
	}
	<-done

	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("stdin→daemon corrupted: got %d bytes, want %d", got.Len(), len(want))
	}
}

// TestBridgeFlushesStdoutBeforeExiting covers the daemon→stdout direction AND
// the flush-on-close behaviour. The reader and the writer are separate
// goroutines now, so a naive teardown would drop whatever was still queued —
// i.e. lose the last screenful of output on every clean disconnect.
func TestBridgeFlushesStdoutBeforeExiting(t *testing.T) {
	client, daemon := net.Pipe()
	defer client.Close()

	want := make([]byte, 96*1024)
	for i := range want {
		want[i] = byte(i % 251)
	}

	// stdin stays open so the ONLY thing ending the bridge is the daemon close.
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()

	go func() {
		_, _ = daemon.Write(want)
		daemon.Close()
	}()

	var got syncBuffer
	if !runBridge(client, stdin, &got) {
		t.Fatal("bridge did not exit when the daemon closed")
	}

	if g := got.Bytes(); !bytes.Equal(g, want) {
		t.Errorf("daemon→stdout lost or corrupted output on teardown: got %d bytes, want %d",
			len(g), len(want))
	}
}

// syncBuffer is a bytes.Buffer safe for the bridge's writer goroutine to append
// to while the test reads it after teardown.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}
