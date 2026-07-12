package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

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
