package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
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
