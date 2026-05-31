// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"time"
)

// defaultPreAuthReadDeadline bounds how long a TCP client can sit on
// an inflight slot before sending its Attach frame. Without this, an
// attacker who opens MaxInflight TCP connections and never writes a
// byte holds every slot indefinitely, denying service to legitimate
// clients. QUIC has handshake/idle timers that close this off
// implicitly; plain TCP doesn't, so we set one explicitly. The
// deadline is cleared by ProtocolHandler.HandleConnection once a
// valid Attach has been read. Five seconds is generous for a healthy
// client (Attach is the first thing on the wire after TCP connect)
// and short enough that slowloris-style holds are economically
// pointless. Overridable via TCPConfig.PreAuthReadDeadline.
const defaultPreAuthReadDeadline = 5 * time.Second

// TCPServer is the plain-TCP variant of Server. Accepts TCP
// connections and drives the same ProtocolHandler that the QUIC
// server uses, wrapping each net.Conn in a tcpAdapter so the
// protocol layer doesn't see transport-specific types.
//
// Why no TLS:
//   TCPServer is intended for use inside a Tailscale tailnet, where
//   WireGuard provides authenticated, encrypted end-to-end transport
//   between the iOS app's in-process tsnet node and the daemon's
//   tsnet-routable address. Wire-level TLS would be redundant — the
//   security model relies on WireGuard's same-or-stronger guarantees.
//   The mtRoam attach-token flow continues to authenticate the client
//   to the daemon at the protocol layer (single-use, expiring token
//   bootstrapped via SSH), so a compromised tsnet node can't replay
//   into a different session.
//
//   When the daemon is exposed on the public internet — no tailnet
//   — operators SHOULD leave the TCP listener off and use the QUIC
//   listener, which carries TLS 1.3 natively. TCP is off by default.
//
// Off-by-default lifecycle:
//   NewTCPServer is only invoked when cfg.TCPAddr is non-empty in the
//   daemon config. The --mtroam-tcp-port CLI flag (default "")
//   threads through to that field. If unset, the daemon ships
//   today's QUIC-only posture.
type TCPServer struct {
	listener            net.Listener
	handler             Handler
	logger              *slog.Logger
	inflight            chan struct{}
	preAuthReadDeadline time.Duration
}

// TCPConfig parallels transport.Config but without TLS or
// QUIC-specific knobs. Tunes the TCP-only listener.
type TCPConfig struct {
	// Addr to listen on (e.g. "127.0.0.1:0" for ephemeral,
	// ":49920" for a fixed port on all interfaces).
	Addr string

	// StateDir is the daemon data directory where the sticky
	// tcp-port file is read/written. Empty disables stickiness.
	StateDir string

	// Handler receives accepted connections wrapped as Conn.
	// Same handler can be shared with a QUIC Server.
	Handler Handler

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger

	// MaxInflight bounds concurrent handlers. Defaults to 64 if
	// zero. Matches the QUIC server's backpressure shape.
	MaxInflight int

	// PreAuthReadDeadline is the per-connection budget for the
	// first Attach frame to arrive. Defaults to 5s if zero. Tests
	// override this to run fast.
	PreAuthReadDeadline time.Duration
}

// NewTCPServer binds the listener with port-walk fallback. Returns
// (nil, nil) if the entire port range is exhausted — the caller
// should log a warning and continue without a TCP server (QUIC and
// SSH tunnelling still work). Returns a non-nil error only for hard
// failures (permission denied, malformed address, etc.).
func NewTCPServer(cfg TCPConfig) (*TCPServer, error) {
	if cfg.Handler == nil {
		return nil, errors.New("transport.NewTCPServer: Handler required")
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 64
	}
	if cfg.PreAuthReadDeadline <= 0 {
		cfg.PreAuthReadDeadline = defaultPreAuthReadDeadline
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	listener, err := bindTCPWithFallback(cfg.Addr, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if listener == nil {
		return nil, nil
	}
	return &TCPServer{
		listener:            listener,
		handler:             cfg.Handler,
		logger:              logger,
		inflight:            make(chan struct{}, cfg.MaxInflight),
		preAuthReadDeadline: cfg.PreAuthReadDeadline,
	}, nil
}

// bindTCPWithFallback mirrors bindUDPWithFallback for the TCP
// listener. Walks a port range on EADDRINUSE, with stickiness via
// the tcp-port state file. Returns (nil, nil) on range exhaustion
// — TCP is optional (only used for Tailscale embedded path).
func bindTCPWithFallback(addr string, stateDir string) (net.Listener, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve tcp addr %q: %w", addr, err)
	}

	if tcpAddr.Port == 0 {
		ln, err := net.ListenTCP("tcp", tcpAddr)
		if err != nil {
			return nil, fmt.Errorf("listen tcp: %w", err)
		}
		return ln, nil
	}

	prefPort := uint16(tcpAddr.Port)
	stuck := readTCPPortState(stateDir)
	candidates := buildCandidatePortsWithDefault(prefPort, stuck, DefaultTCPPort)

	for _, p := range candidates {
		try := *tcpAddr
		try.Port = int(p)
		ln, err := net.ListenTCP("tcp", &try)
		if err == nil {
			switch {
			case p == stuck && stuck != prefPort:
				slog.Info("transport: bound persisted (sticky) TCP port",
					"preferred", prefPort, "bound", p)
			case p != prefPort:
				slog.Info("transport: preferred TCP port unavailable, fell through",
					"preferred", prefPort, "bound", p)
			}
			writeTCPPortState(stateDir, p)
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("listen tcp %s: %w", try.String(), err)
		}
	}

	slog.Warn("transport: no free TCP port in range, TCP listener disabled",
		"from", prefPort, "to", prefPort+FallbackPortSpan)
	return nil, nil
}

// Addr returns the listener's bound local TCP address. Useful for
// logging and for daemon doctor's mtroam_tcp_addr field.
func (s *TCPServer) Addr() *net.TCPAddr {
	return s.listener.Addr().(*net.TCPAddr)
}

// Serve accepts connections until ctx cancels or the listener
// closes (via Close()). Per-conn handler invocations run in their
// own goroutines, bounded by MaxInflight.
//
// Why ctx-cancel triggers a Close goroutine:
//   net.Listener.Accept doesn't honour context the way quic-go's
//   does. The pattern matches the standard library's http.Server.
//   Daemon Shutdown calls Close() explicitly anyway; this is the
//   belt for the suspenders.
func (s *TCPServer) Serve(ctx context.Context) error {
	// Closer goroutine: when ctx cancels, close the listener so
	// the Accept loop unwinds.
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("tcp accept: %w", err)
		}
		select {
		case s.inflight <- struct{}{}:
			go func(nc net.Conn) {
				defer func() { <-s.inflight }()
				// Pre-auth deadline kicks in before any bytes are
				// read. The handler clears it once the Attach frame
				// has been consumed; until then, an idle client
				// holds its slot for at most preAuthReadDeadline.
				_ = nc.SetReadDeadline(time.Now().Add(s.preAuthReadDeadline))
				s.handler.HandleConnection(ctx, NewTCPAdapter(nc))
			}(conn)
		default:
			// At capacity: refuse with a bare close. The peer
			// observes a connection reset; clients retry. No
			// typed protocol error here — the mtRoam protocol's
			// auth handshake hasn't started yet.
			s.logger.WarnContext(ctx, "tcp: dropping connection (max inflight reached)",
				"remote", conn.RemoteAddr().String())
			_ = conn.Close()
		}
	}
}

// Close stops the listener. Safe to call multiple times.
func (s *TCPServer) Close() error {
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}
