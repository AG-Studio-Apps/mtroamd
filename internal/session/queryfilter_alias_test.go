package session

import (
	"io"
	"strings"
	"testing"
)

// ★★ QueryFilter must COPY the partial escape it carries across chunks.
//
// Pump reads into a single reused 8 KiB buffer (`chunk := make([]byte, 8*1024)`, reused
// every iteration) and hands `chunk[:n]` straight to Process. When a sequence straddles a
// read boundary the filter parks the tail in `pending`; if that is a slice of the caller's
// buffer, the NEXT read overwrites the very bytes it saved, and the sequence is rebuilt
// from whatever landed there instead.
//
// The damage is not confined to the daemon's model: Process's output goes to the ring
// buffer as well, so the client receives the corruption too and renders the escape as
// literal text on screen. This reproduces the aliasing directly, with the same
// reuse-the-buffer pattern Pump uses.
func TestQueryFilterCopiesPendingAcrossAReusedBuffer(t *testing.T) {
	const (
		head = "hello\x1b[5" // read 1 ends mid-CSI
		tail = ";1Hworld"    // read 2 completes it
	)
	buf := make([]byte, 8*1024) // the caller's REUSED read buffer, as in Pump

	f := NewQueryFilter(io.Discard)
	n := copy(buf, head)
	out := append([]byte(nil), f.Process(buf[:n])...)
	// The caller now reuses the same buffer for the next read, as Pump does.
	n = copy(buf, tail)
	out = append(out, f.Process(buf[:n])...)

	got := string(out)
	want := head + tail
	if got != want {
		t.Errorf("split sequence corrupted across a reused buffer:\n got  %q\n want %q", got, want)
	}
	if strings.Contains(got, "[5;1Hworld") && !strings.Contains(got, "\x1b[5;1Hworld") {
		t.Error("the escape introducer was lost, so the client would render the CSI as " +
			"literal text and the screen model would record it as glyphs")
	}
}

// The same, for the OSC path, which parks `pending` at its own site.
func TestQueryFilterCopiesPendingOSCAcrossAReusedBuffer(t *testing.T) {
	const (
		head = "x\x1b]2;my-ti" // partial OSC, no terminator yet
		tail = "tle\x07y"
	)
	buf := make([]byte, 8*1024)
	f := NewQueryFilter(io.Discard)
	n := copy(buf, head)
	out := append([]byte(nil), f.Process(buf[:n])...)
	n = copy(buf, tail)
	out = append(out, f.Process(buf[:n])...)

	if got, want := string(out), head+tail; got != want {
		t.Errorf("split OSC corrupted across a reused buffer:\n got  %q\n want %q", got, want)
	}
}
