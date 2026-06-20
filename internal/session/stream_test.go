package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestStreamSession_ToyClockProducer is the seam's "generic" acceptance test:
// a TOTALLY non-agent producer (a clock emitting timestamp frames) drives the
// stream primitive end-to-end. If a toy like this works naturally — replay,
// follow-from-cursor, and close — the side-channel is genuinely generic and not
// agent-shaped.
func TestStreamSession_ToyClockProducer(t *testing.T) {
	id, _ := NewSessionID()
	s, err := NewStreamSession(id, "clock", 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsStreamBacked() {
		t.Fatal("expected a stream-backed session")
	}

	const ticks = 5
	// Producer: emit N opaque "tick" frames then close. Nothing agent here.
	go func() {
		for i := 0; i < ticks; i++ {
			s.PublishFrame([]byte(fmt.Sprintf("tick %d", i)))
		}
		s.CloseStream()
	}()

	// Consumer: follow from the start, accumulating via the absolute cursor
	// until the stream closes — the same replay-then-live loop a real client
	// attach drives.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got [][]byte
	cursor := 0
	for {
		frames, next, closed, err := s.ReadFramesSince(ctx, cursor)
		if err != nil {
			t.Fatalf("ReadFramesSince: %v", err)
		}
		got = append(got, frames...)
		cursor = next
		if closed {
			break
		}
	}
	if len(got) != ticks {
		t.Fatalf("got %d frames, want %d", len(got), ticks)
	}
	for i, f := range got {
		if want := fmt.Sprintf("tick %d", i); string(f) != want {
			t.Errorf("frame %d = %q, want %q", i, f, want)
		}
	}
}

// TestStreamSession_ReplayAndFollow checks the cursor contract: a late reader
// replays history from 0, and a second read blocks until a new frame arrives.
func TestStreamSession_ReplayAndFollow(t *testing.T) {
	id, _ := NewSessionID()
	s, _ := NewStreamSession(id, "s", 0, time.Minute)

	s.PublishFrame([]byte("a"))
	s.PublishFrame([]byte("b"))

	// Replay from 0 returns both, non-blocking.
	ctx := context.Background()
	frames, next, closed, err := s.ReadFramesSince(ctx, 0)
	if err != nil || closed || len(frames) != 2 || next != 2 {
		t.Fatalf("replay: frames=%d next=%d closed=%v err=%v", len(frames), next, closed, err)
	}

	// Following from the cursor blocks until a new frame is published.
	var wg sync.WaitGroup
	wg.Add(1)
	var live [][]byte
	go func() {
		defer wg.Done()
		fctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		live, _, _, _ = s.ReadFramesSince(fctx, next)
	}()
	// Give the reader a beat to block, then publish.
	time.Sleep(20 * time.Millisecond)
	s.PublishFrame([]byte("c"))
	wg.Wait()
	if len(live) != 1 || string(live[0]) != "c" {
		t.Fatalf("follow: got %v, want [c]", live)
	}
}

// TestStreamSession_InputSink checks the reverse channel: a toy echo producer
// receives client input, and Send* fails cleanly with no sink / after close.
func TestStreamSession_InputSink(t *testing.T) {
	id, _ := NewSessionID()
	s, _ := NewStreamSession(id, "echo", 0, time.Minute)

	// No sink installed yet → clean error, not a panic.
	if err := s.SendInput([]byte("hi")); err == nil {
		t.Fatal("expected error sending input with no sink")
	}

	got := make(chan string, 1)
	s.SetInputSink(func(b []byte) error { got <- string(b); return nil })
	if err := s.SendInput([]byte("hello")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if v := <-got; v != "hello" {
		t.Fatalf("sink got %q, want hello", v)
	}

	// A control sink with an opaque kind + body.
	ctrl := make(chan string, 1)
	s.SetControlSink(func(kind string, body []byte) error { ctrl <- kind + ":" + string(body); return nil })
	if err := s.SendControl("ping", []byte("x")); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	if v := <-ctrl; v != "ping:x" {
		t.Fatalf("control got %q, want ping:x", v)
	}

	// After close, Send* refuses.
	s.CloseStream()
	if err := s.SendInput([]byte("late")); err == nil {
		t.Fatal("expected error sending input after close")
	}
}
