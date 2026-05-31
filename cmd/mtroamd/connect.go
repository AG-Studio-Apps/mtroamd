package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/build"
	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
)

// Exit codes for `mtroamd connect`, matching docs/mtroam-protocol.md
// § 4.4 so iOS-side detection can branch on them deterministically.
const (
	connectExitOK              = 0
	connectExitGenericError    = 1
	connectExitDaemonNotRunning = 2
	connectExitUnknownSession  = 3
	connectExitCapacity        = 4
)

// runConnect is the SSH-side helper. It dials the daemon's unix
// socket, sends an AllocateRequest, prints the bootstrap line on
// stdout, and exits.
func runConnect(args []string) int {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	sessionID := fs.String("session", "new", "session id (32 hex chars) to reattach, or 'new' for a fresh session")
	rows := fs.Uint("rows", 24, "initial PTY rows (new sessions only)")
	cols := fs.Uint("cols", 80, "initial PTY cols (new sessions only)")
	exec := fs.String("exec", "", "command to run inside the new session (default: user's $SHELL)")
	shell := fs.String("shell", "", "override the user's shell for new sessions")
	socket := fs.String("socket", "", "unix socket path (default: $XDG_RUNTIME_DIR/mtroamd.sock)")
	timeout := fs.Duration("timeout", 5*time.Second, "max time to wait for the daemon to respond")
	idleTimeout := fs.Duration("idle-timeout", 0,
		"per-session idle timeout — how long the daemon keeps this session alive while no client is attached "+
			"and the shell is producing no output. 0 = use the daemon's default. A negative value (e.g. -1s) means "+
			"\"Never\" — the session opts out of GC and lives until the daemon dies (SIGTERM, reboot, update, or "+
			"crash). Applied on both fresh spawns AND on reattach: passing a different value than the existing "+
			"session's timeout updates it in place (this is how the iOS Keep-alive picker change reaches an "+
			"already-running session). Clamped at the daemon's --max-idle-timeout ceiling when set, including "+
			"the \"Never\" path so a shared-host operator's policy isn't bypassed.")
	name := fs.String("name", "",
		"user-visible session name. With --session=new (or omitted), enables 'create-if-missing': the daemon "+
			"reattaches to an existing session of this name, or spawns a fresh one with this name. With "+
			"--session=<hex>, --name is ignored (the session's identity is fixed at creation).")
	persist := fs.Bool("persist", false,
		"opt this session into cross-restart persistence. The daemon snapshots the scrollback + metadata to "+
			"~/.local/share/mtroamd/sessions/ so the session survives daemon updates and host reboots. "+
			"Mutually exclusive with --no-persist. When neither is set, the daemon's --persistence-default "+
			"applies. Ignored on reattach to an existing session (persistence is fixed at spawn).")
	noPersist := fs.Bool("no-persist", false,
		"opt this session OUT of cross-restart persistence. Use on hosts where --persistence-default is on "+
			"but a specific session is sensitive (e.g. one-off commands you don't want lingering on disk).")
	stdio := fs.Bool("stdio", false,
		"keep the process running and speak the MTRoam wire protocol over stdin/stdout. "+
			"The iOS client uses this to tunnel MTRoam through the SSH exec channel — no separate "+
			"QUIC/TCP connection or firewall hole needed.")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: mtroamd connect [flags]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	socketPath := *socket
	if socketPath == "" {
		socketPath = discoverClientSocketPath()
	}

	if *rows > 65535 || *cols > 65535 {
		fmt.Fprintln(os.Stderr, "mtroamd connect: rows/cols out of range")
		return connectExitGenericError
	}

	var execArgs []string
	if *exec != "" {
		// Split on whitespace. We accept the simple case of
		// `--exec "tmux new -A -s default"` rather than asking the
		// caller to repeat the flag. Quoting beyond simple
		// whitespace is out of scope; if you need it, set $SHELL
		// to a wrapper script.
		execArgs = strings.Fields(*exec)
	}

	client := ipc.NewClient(socketPath, *timeout)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Resolve --persist / --no-persist into the tri-state *bool the
	// IPC request expects. Both set is a usage error; neither set
	// leaves the field nil so the daemon's default applies.
	if *persist && *noPersist {
		fmt.Fprintln(os.Stderr, "mtroamd connect: --persist and --no-persist are mutually exclusive")
		return connectExitGenericError
	}
	var persistPtr *bool
	if *persist {
		v := true
		persistPtr = &v
	} else if *noPersist {
		v := false
		persistPtr = &v
	}

	resp, err := client.Allocate(ctx, ipc.AllocateRequest{
		SessionID:        *sessionID,
		Rows:             uint16(*rows),
		Cols:             uint16(*cols),
		Exec:             execArgs,
		Shell:            *shell,
		IdleTimeoutNanos: int64(*idleTimeout),
		Name:             *name,
		Persist:          persistPtr,
	})
	if err != nil {
		if errors.Is(err, ipc.ErrDaemonNotRunning) {
			fmt.Fprintf(os.Stderr, "mtroamd connect: daemon not running at %s. Start it with `mtroamd serve` first.\n", socketPath)
			return connectExitDaemonNotRunning
		}
		fmt.Fprintf(os.Stderr, "mtroamd connect: %v\n", err)
		return connectExitGenericError
	}

	if !resp.Ok {
		fmt.Fprintf(os.Stderr, "mtroamd connect: %s: %s\n", resp.Err, resp.Msg)
		switch resp.Err {
		case ipc.ErrUnknownSession:
			return connectExitUnknownSession
		case ipc.ErrCapacity:
			return connectExitCapacity
		default:
			return connectExitGenericError
		}
	}

	if *stdio {
		return runStdioMode(&resp)
	}

	// Print the bootstrap line per docs/mtroam-protocol.md § 4.2:
	//   MTRM_QUIC <version> <port> <session_id> <cert_fp> <attach_token>\n
	fmt.Printf("MTRM_QUIC 1 %d %s %s %s\n",
		resp.Port, resp.SessionID, resp.CertFP, resp.AttachToken)

	// If the daemon was started with --mtroam-tcp-addr, emit a parallel
	// MTRM_TCP line carrying the TCP port. iOS clients in embedded-
	// Tailscale routing mode parse this to learn where to dial via
	// their in-process tsnet — they can't run Apple's QUIC over the
	// TailscaleKit userspace fd, so a TCP variant of the MTRoam
	// transport handles that path. Older clients (no embedded
	// mode) ignore the line.
	//
	// Format intentionally minimal: just version + port. The
	// session_id / cert_fp / attach_token from MTRM_QUIC apply
	// to either transport — MTRoam framing is identical on both,
	// and the attach-token authenticates the client to the
	// daemon at the protocol layer regardless of how the bytes
	// got there. WireGuard provides the wire-level encryption
	// for the TCP path (used inside a tailnet only).
	if resp.TCPPort != 0 {
		fmt.Printf("MTRM_TCP 1 %d\n", resp.TCPPort)
	}

	// Emit the daemon version on a second line so the iOS client (or
	// mtroam, or anything else parsing connect's stdout) can compare
	// the installed daemon to the version it pins. Forward-compat:
	// older daemons don't emit this; clients treat absence as "version
	// unknown" and skip the update banner. iOS uses this to surface
	// a "Daemon update available" affordance per host without an
	// extra SSH round-trip.
	//
	// Format is deliberately simple — one space, raw build.Version
	// (typically `vX.Y.Z` or `vX.Y.Z-dirty` for dev builds; the
	// client strips any suffix before semver comparison).
	fmt.Printf("MTRM_DAEMON_VERSION %s\n", build.Version)

	return connectExitOK
}

// runStdioMode keeps the process running, speaking the MTRoam wire
// protocol over stdin/stdout. The iOS client tunnels MTRoam through
// the SSH exec channel this way — no separate QUIC/TCP connection
// or firewall hole needed.
//
// Flow:
//  1. Print MTRM_STDIO handshake (session ID + attach token)
//  2. Connect to the daemon's TCP listener on loopback
//  3. Bidirectional copy: stdin → TCP → daemon, daemon → TCP → stdout
//
// The daemon's ProtocolHandler runs the normal Attach/AttachAck
// handshake over the TCP connection; the connect process is a
// transparent byte-level bridge. When either side closes (stdin
// EOF from SSH channel teardown, or daemon disconnects), the
// bridge tears down and the process exits.
func runStdioMode(resp *ipc.AllocateResponse) int {
	port := resp.LoopbackTCPPort
	if port == 0 {
		port = resp.TCPPort
	}
	if port == 0 {
		fmt.Fprintln(os.Stderr, "mtroamd connect --stdio: daemon has no TCP listener available")
		return connectExitGenericError
	}

	fmt.Printf("MTRM_DAEMON_VERSION %s\n", build.Version)
	fmt.Printf("MTRM_STDIO 1 %s %s\n", resp.SessionID, resp.AttachToken)
	os.Stdout.Sync()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mtroamd connect --stdio: dial %s: %v\n", addr, err)
		return connectExitGenericError
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stdin → daemon
	go func() {
		defer cancel()
		_, _ = io.Copy(conn, os.Stdin)
	}()

	// daemon → stdout
	go func() {
		defer cancel()
		_, _ = io.Copy(os.Stdout, conn)
	}()

	<-ctx.Done()
	return connectExitOK
}
