package session

import (
	"context"
	"errors"
	"time"
)

// streamMaxLogBytes bounds a stream-backed session's retained frame history.
// Past this, the oldest frames are dropped (the absolute cursor still advances
// so a slow reader is told it fell behind rather than silently re-reading).
const streamMaxLogBytes = 4 << 20 // 4 MiB

// errStreamSinkUnset / errStreamClosed are returned by Send* when there is no
// producer to forward to, or the stream has ended.
var (
	errStreamSinkUnset = errors.New("session: stream input/control sink not set")
	errStreamClosed    = errors.New("session: stream closed")
)

// NewStreamSession creates a STREAM-backed session: it has no PTY, and its
// output is supplied by the embedder via PublishFrame rather than read from a
// pty by Pump. The frames are opaque to the core — it only buffers and replays
// them to attachers. Parallel to NewSession (the PTY constructor); a stream
// session uses the same registry / attach / idle-GC machinery, but the daemon
// must NOT call Pump on it (there is no pty to read).
//
// This is a generic primitive: nothing here is specific to any particular kind
// of producer. An embedder hangs its producer state off Session.Ext and drives
// the stream with PublishFrame / SetInputSink.
func NewStreamSession(id SessionID, name string, bufCapacity int, idleTimeout time.Duration) (*Session, error) {
	if bufCapacity <= 0 {
		bufCapacity = DefaultBufferCapacity
	}
	// A stream session keeps a ring buffer too so the shared attach
	// machinery (which may consult buf) never nil-derefs; stream output
	// itself rides frameLog, not buf.
	buf, err := NewRingBuffer(bufCapacity)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &Session{
		id:                 id,
		name:               name,
		created:            now,
		cap:                bufCapacity,
		buf:                buf,
		idleTimeout:        idleTimeout,
		lastActiveAt:       now,
		firstAttachPending: true,
		streamBacked:       true,
	}, nil
}

// IsStreamBacked reports whether this session's output is supplied by
// PublishFrame (vs a PTY). Set once at construction; lock-free read.
func (s *Session) IsStreamBacked() bool { return s.streamBacked }

// PublishFrame appends one opaque frame to the session's bounded frame log and
// wakes any blocked readers. Intended to be called by a SINGLE producer
// goroutine. Oversized history is trimmed front-first (drop-oldest); the
// absolute cursor base (frameDropped) advances so a lagging reader is clamped
// forward rather than silently re-reading evicted frames.
func (s *Session) PublishFrame(body []byte) {
	s.streamMu.Lock()
	s.frameLog = append(s.frameLog, body)
	s.frameLogBytes += len(body)
	for s.frameLogBytes > streamMaxLogBytes && len(s.frameLog) > 1 {
		s.frameLogBytes -= len(s.frameLog[0])
		s.frameLog[0] = nil
		s.frameLog = s.frameLog[1:]
		s.frameDropped++
	}
	s.wakeStreamReadersLocked()
	s.streamMu.Unlock()

	// Keep the session alive: a producing-but-unattended stream must not be
	// reaped by the idle GC. Done OUTSIDE streamMu (touches s.mu state).
	s.mu.Lock()
	s.lastActiveAt = time.Now()
	s.mu.Unlock()
}

// ReadFramesSince returns frames from absolute cursor `from`, blocking until at
// least one is available, the stream closes, or ctx is cancelled. Multiple
// readers may follow at independent cursors, losslessly, at their own pace
// (no per-reader channel) — `next` is the absolute index to pass on the next
// call. `from == 0` replays the full retained history.
func (s *Session) ReadFramesSince(ctx context.Context, from int) (frames [][]byte, next int, closed bool, err error) {
	for {
		s.streamMu.Lock()
		// A reader that fell behind the drop point skips the lost span.
		if from < s.frameDropped {
			from = s.frameDropped
		}
		offset := from - s.frameDropped
		if offset < len(s.frameLog) {
			out := make([][]byte, len(s.frameLog)-offset)
			copy(out, s.frameLog[offset:])
			next = s.frameDropped + len(s.frameLog)
			s.streamMu.Unlock()
			return out, next, false, nil
		}
		if s.streamClosed {
			s.streamMu.Unlock()
			return nil, from, true, nil
		}
		if s.streamNotify == nil {
			s.streamNotify = make(chan struct{})
		}
		wait := s.streamNotify
		s.streamMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, from, false, ctx.Err()
		case <-wait:
			// Woken by PublishFrame/CloseStream; re-evaluate.
		}
	}
}

// CloseStream marks the stream ended and wakes blocked readers (which then
// return closed=true once they've drained any remaining frames). Idempotent.
func (s *Session) CloseStream() {
	s.streamMu.Lock()
	s.streamClosed = true
	s.wakeStreamReadersLocked()
	s.streamMu.Unlock()
}

// wakeStreamReadersLocked closes the current notify channel (waking every
// blocked reader) and clears it so the next waiter installs a fresh one.
// Caller holds streamMu.
func (s *Session) wakeStreamReadersLocked() {
	if s.streamNotify != nil {
		close(s.streamNotify)
		s.streamNotify = nil
	}
}

// SetInputSink installs (or clears, with nil) the reverse channel that forwards
// client input to the producer. Called by the embedder at spawn / teardown.
func (s *Session) SetInputSink(fn func([]byte) error) {
	s.streamMu.Lock()
	s.inputSink = fn
	s.streamMu.Unlock()
}

// SendInput forwards client input to the producer via the input sink. Errors if
// no sink is set or the stream has closed.
func (s *Session) SendInput(data []byte) error {
	s.streamMu.Lock()
	sink, closed := s.inputSink, s.streamClosed
	s.streamMu.Unlock()
	if closed {
		return errStreamClosed
	}
	if sink == nil {
		return errStreamSinkUnset
	}
	return sink(data)
}

// SetControlSink installs (or clears) the out-of-band control channel.
func (s *Session) SetControlSink(fn func(kind string, body []byte) error) {
	s.streamMu.Lock()
	s.controlSink = fn
	s.streamMu.Unlock()
}

// SendControl forwards an out-of-band control message (an opaque kind + body)
// to the producer via the control sink.
func (s *Session) SendControl(kind string, body []byte) error {
	s.streamMu.Lock()
	sink, closed := s.controlSink, s.streamClosed
	s.streamMu.Unlock()
	if closed {
		return errStreamClosed
	}
	if sink == nil {
		return errStreamSinkUnset
	}
	return sink(kind, body)
}
