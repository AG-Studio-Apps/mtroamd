// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"context"
	"net"
	"testing"
	"time"
)

// stubBlockingHandler stands in for the real ProtocolHandler in
// TCPServer tests. It blocks on `Read` from the connection — what
// every real client would do while waiting for the peer's first
// frame — and signals each invocation's return on `done`. With the
// pre-auth deadline active the Read returns a timeout error, the
// handler exits, and the inflight slot is released. Without the
// deadline, the goroutine would block forever and the test would
// hang.
type stubBlockingHandler struct {
	done chan struct{}
}

func (h *stubBlockingHandler) HandleConnection(_ context.Context, c Conn) {
	buf := make([]byte, 1)
	_, _ = c.Read(buf) // returns on deadline (timeout) or peer-close.
	h.done <- struct{}{}
}

// TestTCPServer_PreAuthDeadline_ReapsIdleClients verifies the
// slowloris guard: an attacker who opens MaxInflight TCP connections
// without sending the Attach frame should NOT block all subsequent
// legitimate clients indefinitely. Each idle connection holds its
// inflight slot for at most TCPConfig.PreAuthReadDeadline.
func TestTCPServer_PreAuthDeadline_ReapsIdleClients(t *testing.T) {
	const maxInflight = 4
	const deadline = 200 * time.Millisecond

	h := &stubBlockingHandler{done: make(chan struct{}, maxInflight+1)}
	srv, err := NewTCPServer(TCPConfig{
		Addr:                "127.0.0.1:0",
		Handler:             h,
		MaxInflight:         maxInflight,
		PreAuthReadDeadline: deadline,
	})
	if err != nil {
		t.Fatalf("NewTCPServer: %v", err)
	}
	defer srv.Close() //nolint:errcheck // best-effort close on cleanup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	addr := srv.Addr().String()

	// Fill every inflight slot with an idle connection. The clients
	// don't write a byte; their server-side counterpart sits in Read.
	idle := make([]net.Conn, 0, maxInflight)
	for i := 0; i < maxInflight; i++ {
		c, dialErr := net.Dial("tcp", addr)
		if dialErr != nil {
			t.Fatalf("idle dial #%d: %v", i, dialErr)
		}
		idle = append(idle, c)
	}
	defer func() {
		for _, c := range idle {
			_ = c.Close()
		}
	}()

	// Wait for every server-side handler to finish (each one trips
	// the read deadline and returns). Give a generous grace window
	// over the deadline so the test isn't flaky on a busy CI runner.
	grace := time.After(deadline + 5*time.Second)
	for i := 0; i < maxInflight; i++ {
		select {
		case <-h.done:
			// Handler returned — slot released.
		case <-grace:
			t.Fatalf("idle handler %d not reaped within %v", i, deadline+5*time.Second)
		}
	}

	// Slots should be free now; a fresh dial must be accepted and
	// its handler invoked.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("fresh dial after reap: %v", err)
	}
	defer c.Close() //nolint:errcheck // best-effort

	select {
	case <-h.done:
		// Fresh handler invoked — confirms the inflight slot was reclaimed.
	case <-time.After(deadline + 5*time.Second):
		t.Fatal("fresh handler not invoked after slots should have freed")
	}

	cancel()
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve returned: %v", err)
	}
}
