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
	"github.com/AG-Studio-Apps/mtroamd/internal/svcmgr"
)

// Exit codes for `mtroamd connect`, matching docs/mtroam-protocol.md
// § 4.4 so iOS-side detection can branch on them deterministically.
const (
	connectExitOK               = 0
	connectExitGenericError     = 1
	connectExitDaemonNotRunning = 2
	connectExitUnknownSession   = 3
	connectExitCapacity         = 4
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
		"keep the process running and speak the mtRoam wire protocol over stdin/stdout. "+
			"The iOS client uses this to tunnel mtRoam through the SSH exec channel — no separate "+
			"QUIC/TCP connection or firewall hole needed.")
	autostart := fs.Bool("autostart", true,
		"if the daemon isn't running, auto-start it via its systemd-user service and retry once. "+
			"Only fires when a unit is installed AND the user manager is reachable — it never "+
			"nohup-spawns a daemon (that would bind a public listener with flags the operator "+
			"never chose). Pass --autostart=false for the strict 'error if down' behavior.")
	envFile := fs.String("env-file", "",
		"path to a KEY=VAL env file (one per line, '#' comments) whose vars are added to a NEW "+
			"session's shell environment. The file is READ then DELETED, so secret values never "+
			"appear in this process's argv. Ignored on reattach. The iOS client SFTP-stages a 0600 "+
			"file here to deliver host env vars / secret profiles to mtRoam sessions.")
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

	// Consume the env file (read + delete) into a map for the request.
	// Best-effort: a missing/malformed file logs and connects WITHOUT the
	// extra env rather than failing the whole session over it - the same
	// degrade posture the iOS staging path takes. Always attempt the delete
	// so a staged secret never lingers, even on a parse error.
	var envMap map[string]string
	if *envFile != "" {
		parsed, err := readAndDeleteEnvFile(*envFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mtroamd connect: env-file:", err)
		} else {
			envMap = parsed
		}
	}

	req := ipc.AllocateRequest{
		SessionID:        *sessionID,
		Rows:             uint16(*rows),
		Cols:             uint16(*cols),
		Exec:             execArgs,
		Shell:            *shell,
		IdleTimeoutNanos: int64(*idleTimeout),
		Name:             *name,
		Persist:          persistPtr,
		Env:              envMap,
	}
	resp, err := client.Allocate(ctx, req)
	autoStarted := false
	if errors.Is(err, ipc.ErrDaemonNotRunning) && *autostart {
		// The daemon isn't up — most often a reboot that didn't restart a
		// non-lingering daemon (the visible symptom of the persistence
		// gap). Bring it back via its supervisor and retry once: this
		// self-heals both CLI users and the iOS bootstrap (which shells
		// this command). Persisted sessions rehydrate on restart.
		started, live := autoStartDaemon(socketPath)
		autoStarted = started // "started" (even if the socket never came up) selects the crash message below
		if live {
			client = ipc.NewClient(socketPath, *timeout) // fresh dial to the now-live socket
			ctx2, cancel2 := context.WithTimeout(context.Background(), *timeout)
			defer cancel2()
			resp, err = client.Allocate(ctx2, req)
		}
	}
	if err != nil {
		if errors.Is(err, ipc.ErrDaemonNotRunning) {
			if autoStarted {
				// The supervisor start succeeded but the daemon isn't
				// answering yet: either still binding (a slow cert load can
				// outlast our wait, so a retry succeeds) or it crashed on
				// startup. Cover both without alarming the common slow case.
				fmt.Fprintf(os.Stderr, "mtroamd connect: daemon started but did not come up in time at %s. It may still be starting (retry), or it crashed on startup (check `mtroamd doctor` / the log).\n", socketPath)
			} else {
				fmt.Fprintf(os.Stderr, "mtroamd connect: daemon not running at %s. Start it with `mtroamd serve`, or install the service (see `mtroamd unit`).\n", socketPath)
			}
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
	// TailscaleKit userspace fd, so a TCP variant of the mtRoam
	// transport handles that path. Older clients (no embedded
	// mode) ignore the line.
	//
	// Format intentionally minimal: just version + port. The
	// session_id / cert_fp / attach_token from MTRM_QUIC apply
	// to either transport — mtRoam framing is identical on both,
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

	printSessionReused(&resp)
	printSessionHookInstalled(&resp)
	printBootID(&resp)
	printSessionShimReady(&resp)

	return connectExitOK
}

// printSessionReused emits the optional MTRM_SESSION_REUSED bootstrap
// line: 1 when the allocate landed on an already-running session, 0 for
// a fresh spawn. Only printed when the daemon reported the bit (v1.7.6+;
// an older daemon process behind a newer binary leaves it nil), so
// clients can treat absence as "unknown" and fall back conservatively.
// iOS uses this to decide whether a live env-injection pass is needed
// (reused shell predates --env-file) or redundant (fresh spawn got it).
// Both bootstrap modes emit it BEFORE their handshake line's wire
// switch-over; line-scanning clients ignore unknown prefixes.
func printSessionReused(resp *ipc.AllocateResponse) {
	if resp.Reused == nil {
		return
	}
	v := 0
	if *resp.Reused {
		v = 1
	}
	fmt.Printf("MTRM_SESSION_REUSED %d\n", v)
}

// printBootID emits the optional MTRM_BOOT_ID bootstrap line: the
// daemon process instance id (random hex per daemon start, v1.7.8+).
// Clients key their "broker secrets already delivered" cache on it - a
// changed id means the RAM secret store was wiped and needs a re-push;
// an unchanged one lets a reconnect skip the redundant re-delivery.
// Absent from older daemons; clients treat absence as "always re-push".
func printBootID(resp *ipc.AllocateResponse) {
	if resp.BootID == "" {
		return
	}
	fmt.Printf("MTRM_BOOT_ID %s\n", resp.BootID)
}

// printSessionShimReady emits the optional MTRM_SHIM_READY bootstrap
// line: 1 when the session's shell has the broker shim dir on PATH (a
// brokered secret reaches its tools), 0/absent otherwise. Only printed
// when the daemon reported the bit (v1.7.8+); clients treat absence as
// "unknown" and warn the user to regenerate the session before relying
// on a hidden secret. Mirrors printSessionHookInstalled.
func printSessionShimReady(resp *ipc.AllocateResponse) {
	if resp.ShimReady == nil {
		return
	}
	v := 0
	if *resp.ShimReady {
		v = 1
	}
	fmt.Printf("MTRM_SHIM_READY %d\n", v)
}

// printSessionHookInstalled emits the optional MTRM_LIVE_INJECT
// bootstrap line: 1 when the session's shell has a working live-inject
// prompt hook (bash/zsh, seeded after the user's rc), 0 when it doesn't
// (dash/sh, an unknown shell, or a seeding failure). Only printed when
// the daemon reported the bit (a hook-aware daemon; an older one leaves
// it nil), so clients treat absence as "unknown" and fall back. iOS
// uses it to decide whether SFTPing ~/.mt-inject-<sessionID> will be
// sourced on the next prompt. Like printSessionReused, both bootstrap
// modes emit it BEFORE the MTRM_STDIO wire switch-over; line-scanning
// clients ignore unknown prefixes.
func printSessionHookInstalled(resp *ipc.AllocateResponse) {
	if resp.HookInstalled == nil {
		return
	}
	v := 0
	if *resp.HookInstalled {
		v = 1
	}
	fmt.Printf("MTRM_LIVE_INJECT %d\n", v)
}

// readAndDeleteEnvFile reads KEY=VAL lines (one per line; blank lines and
// '#' comments skipped) into a map, then removes the file. The delete is
// ALWAYS attempted (even on a parse error) so a staged secret file never
// lingers on the host. A line without '=' or with an empty key is skipped
// rather than failing the whole file. Values may contain '=' (only the
// first splits KEY from VALUE), matching the sidecar's own env-file reader.
func readAndDeleteEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	// Best effort remove regardless of read outcome.
	_ = os.Remove(path)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = val
	}
	return env, nil
}

// autoStartDaemon brings a down daemon back via its supervisor, then waits
// for the IPC socket to accept connections. Returns:
//
//	started — the supervisor's Start was invoked successfully (so a
//	          subsequent "not responding" is a startup crash, not "never ran")
//	live    — the socket came up and is ready to dial
//
// Uses its own budget: starting + socket-binding (systemctl start /
// launchctl bootstrap + cert load) can outlast a normal connect timeout.
//
// NOHUP EXCLUDED by design. `svcmgr.Detect` returns a real supervisor
// (systemd-user / launchd — both idempotent + single-instance, launching
// from the unit/plist's own config) OR the nohup fallback. We auto-start
// only via a real supervisor: nohup.Start execs `serve` with hardcoded
// default flags (`--addr 0.0.0.0:49820`), an unconsented public listener on
// flags the operator never chose, and has no single-instance guard — a
// live-but-momentarily-refusing daemon (ECONNREFUSED classed as
// ErrDaemonNotRunning) would get a SECOND `serve` whose ipc.NewServer
// unlinks + rebinds the live socket, orphaning the original's persistent
// sessions. Nohup hosts get the strict error; the iOS installer's
// persistence copy-window is their path to proper supervision. binPath is
// unused by both real backends (they launch from the unit/plist).
func autoStartDaemon(socketPath string) (started, live bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mgr := svcmgr.Detect(ctx)
	if mgr.Name() == "nohup" {
		return false, false
	}
	if err := mgr.Start(ctx, ""); err != nil {
		fmt.Fprintf(os.Stderr, "mtroamd connect: auto-start via %s failed: %v\n", mgr.Name(), err)
		return false, false
	}
	return true, waitForSocket(ctx, socketPath, 10*time.Second)
}

// waitForSocket polls a unix socket until a dial succeeds or the budget /
// context expires.
func waitForSocket(ctx context.Context, path string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runStdioMode keeps the process running, speaking the mtRoam wire
// protocol over stdin/stdout. The iOS client tunnels mtRoam through
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
	// Must precede MTRM_STDIO: everything after that line is raw mtRoam
	// wire bytes, so an appended line would corrupt the stream.
	printSessionReused(resp)
	printSessionHookInstalled(resp)
	printBootID(resp)
	printSessionShimReady(resp)
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
