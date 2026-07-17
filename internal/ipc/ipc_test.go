package ipc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// echoHandler is a Handler whose Allocate just echoes back the
// SessionID with a stub token, and whose Ping echoes the nonce.
type echoHandler struct {
	called int
}

func (h *echoHandler) HandleAllocate(ctx context.Context, req AllocateRequest) AllocateResponse {
	h.called++
	if req.SessionID == "fail" {
		return AllocateResponse{Ok: false, Err: ErrCapacity, Msg: "test failure"}
	}
	return AllocateResponse{
		Ok:          true,
		SessionID:   req.SessionID,
		AttachToken: "token-" + req.SessionID,
		Port:        4242,
		CertFP:      "fp-stub",
	}
}

func (h *echoHandler) HandlePing(ctx context.Context, req PingRequest) PingResponse {
	return PingResponse{Nonce: req.Nonce}
}

// HandleListSessions / HandleKillSession exist only to satisfy the
// Handler interface added when mtroamd grew named multi-sessions.
// The IPC-level tests in this package focus on Allocate/Ping framing
// and lifecycle; daemon-level coverage of List/Kill behaviour lives
// in internal/daemon/daemon_test.go where there's a real registry.
func (h *echoHandler) HandleListSessions(ctx context.Context, _ ListSessionsRequest) ListSessionsResponse {
	return ListSessionsResponse{Ok: true}
}

func (h *echoHandler) HandleKillSession(ctx context.Context, _ KillSessionRequest) KillSessionResponse {
	return KillSessionResponse{Ok: true}
}

func (h *echoHandler) HandleRenameSession(ctx context.Context, req RenameSessionRequest) RenameSessionResponse {
	return RenameSessionResponse{Ok: true, Name: req.NewName}
}

func (h *echoHandler) HandleStatus(ctx context.Context, _ StatusRequest) StatusResponse {
	return StatusResponse{Ok: true, Version: "test", SessionCount: 0, MaxSessions: 100}
}

func (h *echoHandler) HandleSessionSearch(_ context.Context, _ SessionSearchRequest) SessionSearchResponse {
	return SessionSearchResponse{Ok: true}
}

func (h *echoHandler) HandleSetSessionSecrets(_ context.Context, _ SetSessionSecretsRequest) SetSessionSecretsResponse {
	return SetSessionSecretsResponse{Ok: true}
}

func (h *echoHandler) HandleGetSecrets(_ context.Context, req GetSecretsRequest) GetSecretsResponse {
	return GetSecretsResponse{Ok: true, Env: map[string]string{req.Command: "echo"}}
}

func startServer(t *testing.T, h Handler) (*Server, string) {
	t.Helper()
	dir := tempDirWith0700(t)
	socket := filepath.Join(dir, "mtroamd.sock")
	srv, err := NewServer(socket, h)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go srv.Serve(context.Background())
	// Give Serve a moment to enter Accept.
	time.Sleep(20 * time.Millisecond)
	return srv, socket
}

// tempDirWith0700 returns a t.TempDir() chmod'd to 0700. Required
// for tests after audit F5 (NewServer rejects socket parent dirs
// with mode > 0700). The default t.TempDir() leaves system umask
// behaviour, which is typically 0755 — too loose for production.
func tempDirWith0700(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod tempdir: %v", err)
	}
	return dir
}

func TestNewServerRejectsNilHandler(t *testing.T) {
	t.Parallel()
	if _, err := NewServer(filepath.Join(tempDirWith0700(t), "x.sock"), nil); err == nil {
		t.Error("NewServer accepted nil handler")
	}
}

func TestNewServerCreatesSocketWith0600(t *testing.T) {
	t.Parallel()
	dir := tempDirWith0700(t)
	socket := filepath.Join(dir, "mtroamd.sock")
	srv, err := NewServer(socket, &echoHandler{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// Stat the socket; mode should be 0600.
	info, err := osStat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("socket mode = %o, want 0600", mode)
	}
}

func TestAllocateRoundTrip(t *testing.T) {
	t.Parallel()
	h := &echoHandler{}
	_, socket := startServer(t, h)
	c := NewClient(socket, 0)

	resp, err := c.Allocate(context.Background(), AllocateRequest{
		SessionID: "abc123",
		Rows:      24,
		Cols:      80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ok {
		t.Errorf("resp.Ok = false, err=%q msg=%q", resp.Err, resp.Msg)
	}
	if resp.SessionID != "abc123" {
		t.Errorf("SessionID = %q, want abc123", resp.SessionID)
	}
	if resp.AttachToken != "token-abc123" {
		t.Errorf("AttachToken = %q, want token-abc123", resp.AttachToken)
	}
	if resp.Port != 4242 {
		t.Errorf("Port = %d, want 4242", resp.Port)
	}
}

func TestAllocateFailureRoundTrip(t *testing.T) {
	t.Parallel()
	_, socket := startServer(t, &echoHandler{})
	c := NewClient(socket, 0)

	resp, err := c.Allocate(context.Background(), AllocateRequest{SessionID: "fail"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ok {
		t.Error("resp.Ok = true on a request that should have failed")
	}
	if resp.Err != ErrCapacity {
		t.Errorf("Err = %q, want %q", resp.Err, ErrCapacity)
	}
}

func TestPingRoundTrip(t *testing.T) {
	t.Parallel()
	_, socket := startServer(t, &echoHandler{})
	c := NewClient(socket, 0)
	resp, err := c.Ping(context.Background(), 0xdeadbeef)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Nonce != 0xdeadbeef {
		t.Errorf("Nonce = %x, want deadbeef", resp.Nonce)
	}
}

func TestClientReportsDaemonNotRunning(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(t.TempDir(), "nope.sock")
	c := NewClient(socket, 100*time.Millisecond)
	_, err := c.Ping(context.Background(), 1)
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("err = %v, want ErrDaemonNotRunning", err)
	}
}

func TestServeReplacesStaleSocket(t *testing.T) {
	t.Parallel()
	dir := tempDirWith0700(t)
	socket := filepath.Join(dir, "mtroamd.sock")
	// Plant a stale file at the socket path (NOT a real socket).
	// NewServer should remove it and bind cleanly.
	if err := writeFile(socket, "stale", 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(socket, &echoHandler{})
	if err != nil {
		t.Fatalf("NewServer with stale socket: %v", err)
	}
	defer srv.Close()
}

func TestCloseRemovesSocket(t *testing.T) {
	t.Parallel()
	srv, socket := startServer(t, &echoHandler{})
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := osStat(socket); err == nil {
		t.Error("socket file still present after Close")
	}
}

// blockingHandler holds Allocate inside the handler until the test
// signals via release. Used to pin an in-flight handler so the
// overflow path can be exercised deterministically.
type blockingHandler struct {
	echoHandler
	enter   chan struct{}
	release chan struct{}
}

func (h *blockingHandler) HandleAllocate(ctx context.Context, req AllocateRequest) AllocateResponse {
	h.enter <- struct{}{}
	<-h.release
	return h.echoHandler.HandleAllocate(ctx, req)
}

func TestServeRejectsOverCapConnections(t *testing.T) {
	t.Parallel()
	h := &blockingHandler{
		enter:   make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	dir := tempDirWith0700(t)
	socket := filepath.Join(dir, "mtroamd.sock")
	srv, err := NewServer(socket, h, WithMaxConcurrent(1))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go srv.Serve(context.Background())
	time.Sleep(20 * time.Millisecond)

	c := NewClient(socket, 0)

	// First connection: fire-and-forget so it sticks in the handler.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = c.Allocate(context.Background(), AllocateRequest{SessionID: "first"})
	}()

	// Wait for the first request to actually enter the handler. At
	// this point the inflight slot is taken (cap = 1).
	select {
	case <-h.enter:
	case <-time.After(2 * time.Second):
		t.Fatal("first handler never entered")
	}

	// Second connection should be rejected. The server closes the
	// unix conn immediately; the client's ReadFrame surfaces an
	// error rather than a typed response.
	_, err = c.Allocate(context.Background(), AllocateRequest{SessionID: "second"})
	if err == nil {
		t.Error("second request succeeded; expected over-cap rejection")
	}

	// Release the first handler and let it complete.
	close(h.release)
	<-firstDone
}

// waitForInflight polls the server's inflight-slot count until it equals
// want or the timeout elapses. len(chan) is safe to read concurrently, so
// this synchronizes on real server state instead of fixed sleeps.
func waitForInflight(t *testing.T, srv *Server, want int, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(srv.inflight) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: inflight=%d, want %d after %s", msg, len(srv.inflight), want, timeout)
}

// TestStalledConnectionReleasesSlot verifies the F4 fix on the read path:
// a peer that connects but never sends a request frame must not pin its
// handler slot forever. With cap=1 the idle connection takes the sole
// slot; the handler's read deadline must then reclaim it (inflight 1 -> 0)
// so a subsequent real request succeeds. Synchronizes on the inflight
// count, not wall-clock sleeps, so it can't flake: without the fix the
// slot stays held and the 1->0 wait fails deterministically.
//
// The occupancy window is set generously (500ms via WithHandlerTimeout) so
// the first waitForInflight poll reliably observes inflight==1 before the
// deadline reclaims it, even on a preempted CI runner.
//
// NOTE: this covers only the read-stall path. The symmetric write-stall
// (valid request, peer never drains a response big enough to fill the
// socket send buffer) is guarded by the fresh write deadline in
// Server.respond, but is not unit-tested here: forcing a real write block
// requires SO_SNDBUF tuned below the 64 KiB frame cap, which isn't
// portably reproducible without a socket-buffer hook the server doesn't
// expose. The per-write deadline makes the guard structural rather than an
// incidental side effect, so a refactor can't silently drop it.
func TestStalledConnectionReleasesSlot(t *testing.T) {
	t.Parallel()
	dir := tempDirWith0700(t)
	socket := filepath.Join(dir, "mtroamd.sock")
	srv, err := NewServer(socket, &echoHandler{},
		WithMaxConcurrent(1), WithHandlerTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go srv.Serve(context.Background())

	// Raw connection that connects then goes silent — the slot-pinning
	// peer. Wait until the server has accepted it and it occupies the slot.
	idle, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial idle: %v", err)
	}
	defer idle.Close()
	waitForInflight(t, srv, 1, 2*time.Second, "idle conn never occupied the slot")

	// The handler's read deadline must reclaim the slot (this is the
	// regression: without the deadline it stays held forever).
	waitForInflight(t, srv, 0, 3*time.Second, "slot never reclaimed after stalled read")

	// With the slot free, a real request succeeds.
	c := NewClient(socket, time.Second)
	resp, err := c.Allocate(context.Background(), AllocateRequest{SessionID: "afterstall"})
	if err != nil {
		t.Fatalf("allocate after reclaim: %v", err)
	}
	if !resp.Ok {
		t.Fatalf("allocate not ok: %+v", resp)
	}
}
