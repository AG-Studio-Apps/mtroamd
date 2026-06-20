package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// clockExt is a TOY, totally non-agent AllocateExtension: on allocate it spawns
// a stream-backed session and emits a few opaque "tick" frames, then closes.
// Its existence is the seam's acceptance test — if a clock can drive the
// extension+stream path naturally, the seam is genuinely generic (no agent code
// in core). It is the daemon-level analogue of the session-level toy in
// session/stream_test.go.
type clockExt struct{ ticks int }

func (clockExt) Kind() string { return "clock" }

func (c clockExt) Spawn(ctx context.Context, env SpawnEnv, req ipc.AllocateRequest) (*session.Session, error) {
	sid, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	sess, err := session.NewStreamSession(sid, "clock-"+sid.String()[:6], env.BufferBytes, time.Minute)
	if err != nil {
		return nil, err
	}
	if err := env.Registry.Add(sess); err != nil {
		_ = sess.Close()
		return nil, err
	}
	n := c.ticks
	go func() {
		for i := 0; i < n; i++ {
			sess.PublishFrame([]byte(fmt.Sprintf("tick %d", i)))
		}
		sess.CloseStream()
	}()
	return sess, nil
}

// TestAllocateExtension_ToyClock proves the full seam end-to-end through the
// daemon's allocate dispatch: an unknown Kind is rejected (core ships no
// extensions), and the registered toy Kind is dispatched to the extension, which
// produces a stream-backed session — with ZERO agent concepts anywhere.
func TestAllocateExtension_ToyClock(t *testing.T) {
	tmp := shortTempDir(t)
	if err := os.Chmod(tmp, 0o700); err != nil {
		t.Fatalf("chmod tempdir: %v", err)
	}
	d, err := New(Config{
		QUICAddr:           "127.0.0.1:0",
		IPCSocketPath:      filepath.Join(tmp, "mtroamd.sock"),
		CertDir:            tmp,
		IdleTimeout:        time.Hour,
		SidecarStderr:      io.Discard,
		AllocateExtensions: []AllocateExtension{clockExt{ticks: 3}},
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}

	// Unknown kind → rejected (the dispatch is inert without a registration).
	if _, err := d.lookupOrCreateSession(ipc.AllocateRequest{Kind: "nope"}); err == nil {
		t.Fatal("expected unknown allocate kind to error")
	}

	// Registered kind → dispatched to the toy extension.
	sess, err := d.lookupOrCreateSession(ipc.AllocateRequest{Kind: "clock"})
	if err != nil {
		t.Fatalf("clock allocate: %v", err)
	}
	if !sess.IsStreamBacked() {
		t.Fatal("expected a stream-backed session from the extension")
	}

	// Drain the produced frames via the same cursor loop a client attach uses.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got [][]byte
	cursor := 0
	for {
		frames, next, closed, err := sess.ReadFramesSince(ctx, cursor)
		if err != nil {
			t.Fatalf("ReadFramesSince: %v", err)
		}
		got = append(got, frames...)
		cursor = next
		if closed {
			break
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames from the clock extension, want 3", len(got))
	}
}
