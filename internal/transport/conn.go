// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

// Conn is the transport-abstracted connection that ProtocolHandler
// operates on. It bundles a single bidirectional byte stream with the
// connection-level operations the Roam protocol needs. Both QUIC
// (single-stream-per-conn) and plain TCP have adapters; the protocol
// layer above this interface doesn't know which transport is in use.
//
// The stream IS the connection at this level — there's exactly one
// bidi byte channel per Conn. QUIC's multi-stream feature is
// deliberately not used (see ProtocolHandler's doc comment); the
// whole Roam protocol multiplexes over one stream via tagged-frame
// envelopes. That single-stream design is what lets TCP slot in:
// plain TCP IS one byte stream, so a TCP net.Conn satisfies the same
// shape directly.
//
// Why an interface here, instead of teaching ProtocolHandler about
// both transports inline: the Roam wire format (control / stdin /
// stdout tagged frames) is already transport-agnostic — the framing
// helpers in internal/protocol take plain io.Reader / io.Writer.
// QUIC-specific behaviour (RTT stats, application-error-coded
// CloseWithError) is the small surface that needs adapting; an
// interface confines that to two small adapter types instead of
// branching all through the handler.
type Conn interface {
	io.Reader
	io.Writer
	io.Closer

	// SetReadDeadline lets drainBriefly bound the post-Close drain
	// before the connection-level close races the last write off the
	// wire. Both *quic.Stream and net.Conn implement this natively.
	SetReadDeadline(time.Time) error

	// RemoteAddr surfaces the peer's network address for logging
	// and the per-connection log scope.
	RemoteAddr() net.Addr

	// CloseWithError closes the connection with an application-level
	// error code + message. On QUIC this becomes CONNECTION_CLOSE
	// carrying the typed code so the client can map it back to a
	// protocol error class. On plain TCP the code is ignored and
	// the conn is closed immediately — typed errors over TCP are
	// signalled inline via the Roam Goodbye / AttachAck.Err frame
	// fields, which the framing layer writes BEFORE we get here.
	CloseWithError(code uint64, msg string) error

	// SmoothedRTT returns the transport's smoothed round-trip-time
	// estimate. (rtt, true) on QUIC where quic-go tracks RTT
	// continuously; (0, false) on plain TCP, where the kernel
	// doesn't expose its TCP_INFO consistently across platforms.
	// Wedge detection's RTT-baseline computation skips the QUIC
	// path on TCP and falls back to a wall-clock RTT derived from
	// Roam-level Ping / Pong frames.
	SmoothedRTT() (time.Duration, bool)

	// CancelRead / CancelWrite abandon one half of the bidirectional
	// channel. On QUIC this maps to *quic.Stream.CancelRead /
	// CancelWrite with the supplied error code, which lets the peer
	// observe a typed reset on its writer / reader. On TCP there is
	// no half-close-with-error primitive in net.Conn, so both calls
	// close the whole connection — the peer observes EOF either way.
	// Pumps call these from their context-Done cleanup hooks; the
	// "code is purely for QUIC" behaviour matches how the existing
	// pumps already used it (always 0).
	CancelRead(code uint64)
	CancelWrite(code uint64)
}

// NewQUICAdapter wraps a *quic.Conn plus its already-accepted bidi
// stream into a Conn. The caller (Server.Serve) owns the AcceptStream
// timing; once that returns the stream, the adapter owns both halves
// for the rest of the connection's lifetime.
func NewQUICAdapter(conn *quic.Conn, stream *quic.Stream) Conn {
	return &quicAdapter{conn: conn, stream: stream}
}

type quicAdapter struct {
	conn   *quic.Conn
	stream *quic.Stream
}

func (q *quicAdapter) Read(p []byte) (int, error)  { return q.stream.Read(p) }
func (q *quicAdapter) Write(p []byte) (int, error) { return q.stream.Write(p) }
func (q *quicAdapter) Close() error                { return q.stream.Close() }

func (q *quicAdapter) SetReadDeadline(t time.Time) error {
	return q.stream.SetReadDeadline(t)
}

func (q *quicAdapter) RemoteAddr() net.Addr { return q.conn.RemoteAddr() }

func (q *quicAdapter) CloseWithError(code uint64, msg string) error {
	return q.conn.CloseWithError(quic.ApplicationErrorCode(code), msg)
}

func (q *quicAdapter) SmoothedRTT() (time.Duration, bool) {
	return q.conn.ConnectionStats().SmoothedRTT, true
}

func (q *quicAdapter) CancelRead(code uint64) {
	q.stream.CancelRead(quic.StreamErrorCode(code))
}

func (q *quicAdapter) CancelWrite(code uint64) {
	q.stream.CancelWrite(quic.StreamErrorCode(code))
}

// NewTCPAdapter wraps a net.Conn so plain TCP traffic can drive
// ProtocolHandler. The TCP path is used when iOS dials via the
// in-process TailscaleKit OutgoingConnection (which gives the caller
// a kernel-bypassing userspace TCP fd, not a UDP one — that's why we
// can't run Apple's QUIC over it). WireGuard provides transport
// security end-to-end inside the tailnet, so wire-level TLS isn't
// needed; the Roam attach-token flow still authenticates the client
// to the daemon at the protocol layer.
func NewTCPAdapter(conn net.Conn) Conn {
	return &tcpAdapter{conn: conn}
}

type tcpAdapter struct {
	conn net.Conn
}

func (t *tcpAdapter) Read(p []byte) (int, error)  { return t.conn.Read(p) }
func (t *tcpAdapter) Write(p []byte) (int, error) { return t.conn.Write(p) }
func (t *tcpAdapter) Close() error                { return t.conn.Close() }

func (t *tcpAdapter) SetReadDeadline(deadline time.Time) error {
	return t.conn.SetReadDeadline(deadline)
}

func (t *tcpAdapter) RemoteAddr() net.Addr { return t.conn.RemoteAddr() }

// CloseWithError on TCP drops the code + msg. The Roam framing layer
// writes a typed Goodbye / AttachAck.Err frame on the wire BEFORE the
// connection is closed, so the client still receives the structured
// error class. The (code, msg) arguments here only mattered as
// CONNECTION_CLOSE payload on QUIC.
func (t *tcpAdapter) CloseWithError(_ uint64, _ string) error {
	return t.conn.Close()
}

// SmoothedRTT is unavailable on plain TCP. quic-go computes a
// continuously-updated smoothed RTT from QUIC ACKs; net.TCPConn
// doesn't surface TCP_INFO uniformly (Linux exposes it, Darwin /
// iOS sandboxed processes don't). Wedge detection falls back to
// wall-clock Ping/Pong RTT in this branch.
func (t *tcpAdapter) SmoothedRTT() (time.Duration, bool) {
	return 0, false
}

// CancelRead / CancelWrite on TCP collapse to a full Close — net.Conn
// has no half-close-with-error primitive. The pump-Cancel paths
// already tolerated a full close (it surfaces as EOF on the other
// side and unwinds the matched pump), so the semantic is preserved.
func (t *tcpAdapter) CancelRead(_ uint64)  { _ = t.conn.Close() }
func (t *tcpAdapter) CancelWrite(_ uint64) { _ = t.conn.Close() }
