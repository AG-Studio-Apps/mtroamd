// Package daemon orchestrates the long-running pieces of mtroamd:
// the session registry, the QUIC listener, and the unix-socket IPC
// server that `mtroamd connect` talks to.
//
// One Daemon per `mtroamd serve` invocation. Run blocks until the
// passed context is cancelled, at which point everything is drained
// in dependency order: IPC server (no new attaches reservable), QUIC
// listener (no new connections), then the registry's Shutdown
// (closes every live session, freeing PTYs).
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/build"
	"github.com/AG-Studio-Apps/mtroamd/internal/cert"
	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/ptyclient"
	"github.com/AG-Studio-Apps/mtroamd/internal/secret"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
	"github.com/AG-Studio-Apps/mtroamd/internal/transport"
)

// WedgeReportFilename is the basename of the JSONL file where the
// per-session wedge watcher appends de-identified records. Lives
// alongside the daemon's other persistence state under stateDir.
// Stable so `mtroamd wedge-report` can find it without flags.
const WedgeReportFilename = "wedge-events.jsonl"

// Config is the daemon's runtime configuration. Defaults are
// applied for any zero / unset fields.
type Config struct {
	// QUICAddr is the bind address for the QUIC listener. Default
	// "127.0.0.1:0" — loopback only, kernel-chosen port. Operators
	// who want the daemon reachable on a Tailnet IP / LAN address
	// should override explicitly (the systemd unit shipped with the
	// release does this for testing).
	QUICAddr string

	// TCPAddr is the optional bind address for the plain-TCP mtRoam
	// listener. Empty (default) disables the listener — daemon
	// ships QUIC-only, behaving exactly as before. When non-empty,
	// the daemon serves the mtRoam protocol over TCP IN ADDITION to
	// QUIC, on the supplied address. Designed for use inside a
	// Tailscale tailnet where WireGuard provides transport
	// security; see transport.TCPServer doc comment for the
	// when-to-use rationale.
	//
	// Wired from --mtroam-tcp-port at the CLI. iOS clients in
	// embedded-Tailscale mode dial this listener; system /
	// direct mode clients use the QUIC listener.
	TCPAddr string

	// IPCSocketPath is the unix socket `mtroamd connect` dials.
	// Required.
	IPCSocketPath string

	// CertDir is the directory where the daemon's self-signed cert
	// + key are persisted. Defaults to cert.DefaultDir.
	CertDir string

	// MaxSessions caps concurrent live sessions. Defaults to
	// session.DefaultMaxSessions.
	MaxSessions int

	// IdleTimeout is how long a detached session can sit before GC
	// when the client doesn't request its own value. Defaults to
	// session.DefaultIdleTimeout.
	IdleTimeout time.Duration

	// MaxIdleTimeout is the ceiling on per-session timeouts a client
	// may request. Zero means no ceiling — appropriate for the
	// personal-server deployment where one user trusts the daemon
	// they're running. Operators of multi-user / shared mtroamd
	// hosts should set this to bound resource cost from a runaway
	// client requesting a 30-day timeout on every session.
	MaxIdleTimeout time.Duration

	// SessionBufferBytes overrides the per-session output ring buffer
	// capacity. Zero falls back to session.DefaultBufferCapacity
	// (4 MiB). Operators who run long, output-heavy builds and want
	// generous reattach-replay history should raise this; the trade
	// is RAM per live session (one buffer per session). 16-32 MiB is
	// reasonable on a dev box; multi-MiB hits are fine even on a Pi.
	SessionBufferBytes int

	// AllocateExtensions are registered by an embedding binary to handle
	// allocate requests carrying a matching AllocateRequest.Kind. The core
	// (a stock terminal daemon) sets none, so the Kind dispatch is inert.
	// See ext.go / AllocateExtension.
	AllocateExtensions []AllocateExtension

	// PersistenceDefault controls whether new sessions opt into
	// cross-restart persistence when the client didn't specify
	// (AllocateRequest.Persist == nil). True (the default-on
	// posture) matches user-mental-model "of course my work
	// survives." Operators of shared / multi-user hosts can flip
	// this to false via `mtroamd serve --persistence-default off`
	// for privacy-by-default; individual sessions still opt back in
	// with explicit `--persist`.
	//
	// Defaults to true when the field is zero-value (uses the
	// `*bool` pattern so empty Config behaves correctly).
	PersistenceDefault *bool

	// PersistenceFlushInterval is how often each persisted session's
	// background flusher checkpoints its ring buffer to disk. Zero
	// falls back to 30s, which trades durability (lose up to 30s of
	// scrollback on crash) for write amplification (4 MiB buffer × 100
	// sessions × every 30s ≈ 13 MB/s peak even at full saturation,
	// well within any SSD's budget).
	PersistenceFlushInterval time.Duration

	// Logger receives operational logs. Defaults to slog.Default().
	Logger *slog.Logger

	// SidecarStderr is the io.Writer passed through to each spawned
	// sidecar's stderr. nil → os.Stderr (production posture: sidecar
	// logs surface in the daemon's journal). Tests pass io.Discard
	// to drop sidecar stderr so the test binary's stderr fd doesn't
	// stay open past reap (which makes `go test` park at WaitDelay).
	SidecarStderr io.Writer
}

// Daemon owns the lifetime of the long-running server pieces.
type Daemon struct {
	cfg    Config
	logger *slog.Logger
	cert   tls.Certificate
	certFP cert.Fingerprint
	// bootID identifies this daemon process instance (random hex per
	// start). Reported on every Allocate so clients can detect a daemon
	// restart (= RAM secret store wiped) vs a plain reconnect.
	bootID string
	// shimSyncMu serializes the store-update + on-disk shim sync pair in
	// HandleSetSessionSecrets: each is individually safe, but two
	// overlapping pushes for the same session could interleave so the
	// LOSING push's SyncShims runs last and the disk shims no longer
	// match the store (a declared command with no shim = silently
	// token-less; review finding). One daemon-wide mutex - pushes are
	// rare and SyncShims is a handful of tiny file writes.
	shimSyncMu sync.Mutex
	registry   *session.Registry
	// allocateExts maps AllocateRequest.Kind → the embedder extension that
	// handles it. Nil/empty for a stock terminal daemon (no agent/MCP code in
	// core). Populated in New() from Config.AllocateExtensions.
	allocateExts map[string]AllocateExtension
	quic         *transport.Server
	// tcp is the optional plain-TCP listener used by iOS clients
	// in embedded-Tailscale mode. Nil when Config.TCPAddr is "" or
	// when a "tailnet:" sentinel is pending resolution.
	tcp *transport.TCPServer
	// tcpMu guards lazy creation/teardown of tcp by the tailnet
	// poller goroutine. Also guards reads from TCPAddr()/doctorInfo().
	tcpMu sync.Mutex
	// deferredTCPSentinel holds the raw "tailnet:<port>" value when
	// Tailscale wasn't available at startup. Empty when TCP is
	// disabled or already bound. The poller reads this.
	deferredTCPSentinel string
	// tcpBoundIP is the tailnet IP the TCP listener is currently
	// bound to, set by the poller on successful bind. Used to detect
	// Tailscale IP changes (re-key) and disappearance.
	tcpBoundIP net.IP
	// loopbackTCP is an always-on TCP listener bound to 127.0.0.1
	// for the --stdio bridge. Separate from the tailnet TCP listener
	// so SSH tunnel mode works regardless of Tailscale state.
	loopbackTCP *transport.TCPServer
	// mtroamHandler is cached from New() so the poller can create a
	// TCPServer with the same handler after startup.
	mtroamHandler *transport.ProtocolHandler
	ipc           *ipc.Server
	// stateDir is the persistence root resolved at New(). Reused by
	// spawnSession when starting the per-session flusher.
	stateDir string
	// daemonBinary is os.Executable() cached at New(). Re-exec'd as
	// `mtroamd pty-sidecar` for each session's PTY-owning helper.
	daemonBinary string
	// startedAt is set once in New so HandleStatus can compute
	// uptime without keeping a separate state machine.
	startedAt time.Time
	// secrets is the in-memory secret broker (v1.7.6+): per-session
	// secret sets pushed by SetSessionSecrets, delivered to consuming
	// commands at exec time via `secret-exec`. Held in RAM ONLY — never
	// written to the session snapshot, dropped on reap — so there is no
	// on-disk secret footprint and a daemon restart simply loses it
	// (the client re-pushes on reconnect).
	secrets *secret.Store
}

// sessionExtraEnvForID returns the per-session env additions the
// daemon already injected into the in-process pty.SpawnConfig before
// the sidecar split. Centralised here so spawnSession + the
// PTYSpawner closure stay in sync.
func sessionExtraEnvForID(sid session.SessionID) []string {
	return []string{
		"MESHTERM_SESSION_ID=" + sid.String(),
		// MESHTERM_ROAM=1 lets user shells short-circuit auto-tmux
		// blocks in their rc files; see the original comment in
		// spawnSession for the recommended guard form.
		"MESHTERM_ROAM=1",
	}
}

// sessionExtraEnv is the *Session-flavoured wrapper used by the
// lazy-spawn closure where the SessionID lives behind the *Session.
func sessionExtraEnv(sess *session.Session) []string {
	return sessionExtraEnvForID(sess.ID())
}

// sessionShimDir is the ONE place the per-session PATH-shadow shim
// directory layout is defined; shimSpawnEnv (spawn-time PATH) and
// Daemon.shimDirFor (SyncShims target) both derive from it so the two
// can never drift apart (a drift = spawn PATH pointing at a dir SyncShims
// never populates, secrets silently undelivered).
func sessionShimDir(stateDir, sid string) string {
	return filepath.Join(stateDir, "sessions", sid, "shims")
}

// defaultSpawnPATH seeds the child's PATH only when the daemon itself has
// an empty/unset PATH (a bare process launched from a stripped env).
// Without it the emitted PATH would collapse to "<shimdir>:" — shim dir
// plus an empty (=cwd) element — and every command in every session
// would stop resolving (review finding). Mirrors the conventional
// login(1)/systemd default.
const defaultSpawnPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// shimSpawnEnv returns the ExtraEnv additions that put a session's
// PATH-shadow shim dir FIRST on PATH and export MESHTERM_SHIM_DIR. The
// dir itself is created lazily by SyncShims on the first SetSessionSecrets
// (a missing PATH entry is harmless — shells skip it — so the spawn hot
// path takes no mkdir syscall). Prepending to os.Getenv("PATH") matches
// pty.BuildEnv's baseline (PATH is allowlisted) plus the shim dir, so it
// never gives a worse PATH than the child would otherwise get. Added on
// EVERY spawn so a later SetSessionSecrets takes effect with no respawn
// (PATH already includes the dir). Caveats: a login shell that rebuilds
// PATH from profile drops the prepend — adoption then needs one fresh
// window/reconnect (the accepted mtRoam limitation). And a daemon-restart
// RESPAWN passes basePATH="" (the original --env-file env, PATH included,
// was consumed at spawn and is not persisted), so a custom toolchain PATH
// from the env-file is absent until the client reconnects and re-stages —
// the same accepted daemon-restart trade as the RAM secret store.
func shimSpawnEnv(stateDir, sid, basePATH string) []string {
	dir := sessionShimDir(stateDir, sid)
	// Prepend to the EFFECTIVE PATH: a user-supplied PATH (from
	// --env-file) if present, else the daemon's, else a sane default.
	// Prepending (not replacing) means a client that set
	// PATH=$HOME/toolchain/bin:... in its env-file keeps those entries -
	// the shim dir just goes first.
	if basePATH == "" {
		basePATH = os.Getenv("PATH")
	}
	if basePATH == "" {
		basePATH = defaultSpawnPATH
	}
	return []string{
		"MESHTERM_SHIM_DIR=" + dir,
		"PATH=" + dir + string(os.PathListSeparator) + basePATH,
	}
}

// New constructs a Daemon. Loads or generates the TLS cert,
// creates the session registry, builds the QUIC listener and IPC
// server. Does NOT start any goroutines — call Run for that.
func New(cfg Config) (*Daemon, error) {
	if cfg.IPCSocketPath == "" {
		return nil, errors.New("daemon: Config.IPCSocketPath is required")
	}
	if cfg.QUICAddr == "" {
		// Audit F-A (v0.0.2 review): library default is loopback so
		// embedders who forget to set QUICAddr don't accidentally
		// expose the daemon on every interface. The CLI flag's
		// own default (`serve.go --addr`) was already 127.0.0.1:0;
		// this aligns the library with that.
		cfg.QUICAddr = "127.0.0.1:0"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	bootIDBytes := make([]byte, 8)
	if _, err := rand.Read(bootIDBytes); err != nil {
		return nil, fmt.Errorf("generate boot id: %w", err)
	}

	mgr := &cert.Manager{Dir: cfg.CertDir}
	tlsCert, fp, err := mgr.LoadOrGenerate()
	if err != nil {
		return nil, fmt.Errorf("load/generate cert: %w", err)
	}

	reg := session.NewRegistry(cfg.MaxSessions, cfg.IdleTimeout, 0, cfg.MaxIdleTimeout)
	// Broker store created before OnReap so the reap hook can drop a
	// session's secrets (the closure can't reference `d`, declared below).
	secretStore := secret.NewStore()
	// Surface idle-GC reap events in the operational log. Pairs
	// with the session.attach / session.detach events emitted by
	// the transport layer so operators tailing logs see the full
	// lifecycle of a session.
	reg.OnReap = func(s *session.Session) {
		// Drop the session's broker secrets from memory the moment it's
		// reaped - the secret set must never outlive its session.
		secretStore.ClearSession(s.ID().String())
		logger.Info("session.reaped",
			"session", s.ID().String(),
			"name_hash", session.NameHash(s.ID(), s.Name()),
		)
		logger.Debug("session.reaped.name",
			"session", s.ID().String(),
			"name", s.Name(),
		)
	}

	// Persistence wiring. State dir lives next to the cert dir
	// (cert.DefaultDir), so a single ~/.local/share/mtroamd holds
	// everything that survives daemon restart: cert, key, IPC socket,
	// and now per-session subdirs under sessions/.
	stateDir := cfg.CertDir
	if stateDir == "" {
		stateDir, err = cert.DefaultDir()
		if err != nil {
			return nil, fmt.Errorf("resolve state dir: %w", err)
		}
	}
	reg.SetStateDir(stateDir)
	if cfg.PersistenceDefault != nil {
		reg.SetPersistenceDefault(*cfg.PersistenceDefault)
	}
	// (Zero-value Config preserves the registry's default-on posture
	// set in NewRegistry — no setter call needed.)

	d := &Daemon{
		cfg:       cfg,
		logger:    logger,
		cert:      tlsCert,
		certFP:    fp,
		bootID:    hex.EncodeToString(bootIDBytes),
		registry:  reg,
		stateDir:  stateDir,
		startedAt: time.Now(),
		secrets:   secretStore,
	}

	// Index any embedder-registered allocate extensions by Kind. The core
	// registers none, so a stock terminal daemon leaves this empty/nil.
	for _, ext := range cfg.AllocateExtensions {
		k := ext.Kind()
		if k == "" {
			return nil, fmt.Errorf("daemon: AllocateExtension has empty Kind")
		}
		if _, dup := d.allocateExts[k]; dup {
			return nil, fmt.Errorf("daemon: duplicate AllocateExtension kind %q", k)
		}
		if d.allocateExts == nil {
			d.allocateExts = make(map[string]AllocateExtension, len(cfg.AllocateExtensions))
		}
		d.allocateExts[k] = ext
	}

	// Hydrate sessions that were persisted by a prior daemon run.
	// PTYs are NOT spawned here — protocol_handler does that lazily
	// on the first client attach. The flushers, however, are started
	// immediately so any output that arrives once a client attaches
	// gets checkpointed on the normal cadence.
	restored, lerr := session.LoadPersisted(stateDir, reg, logger)
	if lerr != nil {
		logger.Warn("session.persistence.load_failed", "err", lerr.Error())
	}
	if restored > 0 {
		logger.Info("session.persistence.restored", "count", restored)
	}
	wedgeLogPath := ""
	if stateDir != "" {
		wedgeLogPath = filepath.Join(stateDir, WedgeReportFilename)
	}
	for _, sid := range reg.IDs() {
		if s, lookupErr := reg.Lookup(sid); lookupErr == nil {
			if wedgeLogPath != "" {
				s.SetWedgeLogPath(wedgeLogPath)
			}
			if s.Persist() {
				s.StartFlusher(stateDir, cfg.PersistenceFlushInterval, logger)
			}
		}
	}

	// Cache os.Executable() once — we re-exec it as `mtroamd
	// pty-sidecar` for every session's PTY-owning helper process.
	daemonBinary, exeErr := os.Executable()
	if exeErr != nil {
		return nil, fmt.Errorf("os.Executable: %w", exeErr)
	}
	d.daemonBinary = daemonBinary

	// Reattach any sidecars that survived the previous daemon's exit.
	// Per-session pidfile + socket in {stateDir}/sessions/<sid>/ point
	// at live processes; we dial each and inject a sidecar-backed
	// PTY into the corresponding Session. Sessions whose sidecars
	// died (or never had one) fall through to the lazy-spawn path on
	// next attach, same as v0.5.x behaviour.
	if discovered, dErr := ptyclient.Discover(context.Background(), reg, stateDir, logger); dErr != nil {
		logger.Warn("session.sidecar.discovery_failed", "err", dErr.Error())
	} else if discovered > 0 {
		logger.Info("session.sidecar.reattached", "count", discovered)
	}

	// Construct the protocol handler once and share between the
	// QUIC server and the optional TCP server. Same Registry, same
	// session lifecycle, same attach-token plumbing — only the
	// transport differs (QUIC over kernel UDP vs TCP, the latter
	// reached via tsnet's userspace stack on the iOS side).
	// One source limiter shared across the QUIC + TCP listeners and the
	// protocol handler, so per-source accept rate-limiting and bad-token
	// cooldowns count a peer's behaviour across every transport. Defaults
	// (see transport.LimiterConfig) are generous enough that legitimate
	// reconnect bursts pass; they only bite churn/scanning on a
	// network-reachable bind.
	srcLimiter := transport.NewSourceLimiter(transport.LimiterConfig{})

	mtroamHandler := &transport.ProtocolHandler{
		Registry: reg,
		Logger:   logger,
		Limiter:  srcLimiter,
		// PTYSpawner gives protocol_handler a way to lazy-spawn
		// the child shell for a restored session on its first
		// attach. We spawn an out-of-process sidecar so the
		// child shell survives subsequent daemon restarts —
		// see internal/ptysidecar for the design.
		PTYSpawner: func(sess *session.Session, rows, cols uint16) (session.PTY, error) {
			// context.Background here is fine: SpawnNew only uses
			// the ctx for the bounded 3 s dial-with-backoff, and a
			// daemon-shutdown that races a fresh spawn will just
			// see the sidecar disconnect cleanly via socket-close.
			conn, err := ptyclient.SpawnNew(context.Background(), ptyclient.SpawnConfig{
				SessionID:    sess.ID().String(),
				Rows:         rows,
				Cols:         cols,
				ExtraEnv:     append(sessionExtraEnv(sess), shimSpawnEnv(stateDir, sess.ID().String(), "")...),
				StateDir:     stateDir,
				DaemonBinary: daemonBinary,
				Logger:       logger,
				Stderr:       cfg.SidecarStderr,
			})
			if err != nil {
				return nil, err
			}
			// A restored session's shell is respawned fresh here, so the
			// hook is re-seeded: refresh the stored state to the value
			// the new sidecar reported. A later allocate then reports the
			// current truth rather than the pre-restart snapshot.
			sess.SetHookInstalled(conn.HookInstalled())
			// shimReady comes from the SIDECAR (it re-asserts the shim dir
			// on the live PATH in the seeded rcfile and reports whether it
			// is guaranteed), not an assumption that shimSpawnEnv ran - a
			// login shell could otherwise have rebuilt PATH and dropped it.
			sess.SetShimReady(conn.ShimReady())
			return conn, nil
		},
	}

	d.quic, err = transport.New(transport.Config{
		Addr:     cfg.QUICAddr,
		Cert:     tlsCert,
		Handler:  mtroamHandler,
		StateDir: stateDir,
		Limiter:  srcLimiter,
	})
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}

	d.mtroamHandler = mtroamHandler

	d.loopbackTCP, err = transport.NewTCPServer(transport.TCPConfig{
		Addr:     "127.0.0.1:0",
		StateDir: stateDir,
		Handler:  mtroamHandler,
		Logger:   logger,
		Limiter:  srcLimiter,
	})
	if err != nil {
		_ = d.quic.Close()
		return nil, fmt.Errorf("loopback tcp: %w", err)
	}

	if strings.HasPrefix(cfg.TCPAddr, "tailnet:") {
		// Tailscale sentinel: try to resolve now; defer to poller if
		// Tailscale isn't running yet.
		resolved, ip, resolveErr := transport.ResolveBindAddr(cfg.TCPAddr)
		if resolveErr != nil {
			logger.Info("tailnet not available at startup, TCP deferred",
				"sentinel", cfg.TCPAddr, "err", resolveErr)
			d.deferredTCPSentinel = cfg.TCPAddr
		} else {
			logger.Info("mtroam-tcp bound to tailnet interface",
				"sentinel", cfg.TCPAddr, "resolved", resolved, "ip", ip)
			d.tcp, err = transport.NewTCPServer(transport.TCPConfig{
				Addr:     resolved,
				StateDir: stateDir,
				Handler:  mtroamHandler,
				Logger:   logger,
				Limiter:  srcLimiter,
			})
			if err != nil {
				_ = d.quic.Close()
				return nil, fmt.Errorf("tcp transport: %w", err)
			}
			d.tcpBoundIP = ip
		}
	} else if cfg.TCPAddr != "" {
		d.tcp, err = transport.NewTCPServer(transport.TCPConfig{
			Addr:     cfg.TCPAddr,
			StateDir: stateDir,
			Handler:  mtroamHandler,
			Logger:   logger,
			Limiter:  srcLimiter,
		})
		if err != nil {
			_ = d.quic.Close()
			return nil, fmt.Errorf("tcp transport: %w", err)
		}
	}

	d.ipc, err = ipc.NewServer(cfg.IPCSocketPath, d)
	if err != nil {
		_ = d.quic.Close()
		return nil, fmt.Errorf("ipc: %w", err)
	}

	return d, nil
}

// Addr returns the QUIC listener's bound UDP address. Useful for
// tests and for logging "we're ready" lines that include the
// chosen port.
func (d *Daemon) Addr() string { return d.quic.Addr().String() }

// TCPAddr returns the optional mtRoam-over-TCP listener's bound
// address, or "" when the TCP listener is disabled (Config.TCPAddr
// was empty). Surfaced via mtroamd doctor JSON so iOS clients
// in embedded-Tailscale mode can discover the port via the same
// SSH-bootstrap mechanism that learns the QUIC port.
func (d *Daemon) LoopbackTCPAddr() string {
	if d.loopbackTCP == nil {
		return ""
	}
	return d.loopbackTCP.Addr().String()
}

func (d *Daemon) TCPAddr() string {
	d.tcpMu.Lock()
	defer d.tcpMu.Unlock()
	if d.tcp == nil {
		return ""
	}
	return d.tcp.Addr().String()
}

// CertFingerprint returns the SHA-256 fingerprint of the daemon's
// TLS cert — the value the iOS client pins via the bootstrap line.
func (d *Daemon) CertFingerprint() cert.Fingerprint { return d.certFP }

// IPCSocketPath returns the unix socket path the IPC server is
// bound to.
func (d *Daemon) IPCSocketPath() string { return d.ipc.Path() }

// Run drives the registry GC loop, the QUIC listener, and the IPC
// server until ctx is cancelled. Returns the first error any
// component returns, or nil on graceful shutdown.
//
// Shutdown order: cancel ctx → IPC and QUIC servers' Serve loops
// return → registry.Run's deferred Shutdown closes all sessions →
// QUIC listener and IPC socket are closed.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.InfoContext(ctx, "mtroamd starting",
		"quic_addr", d.Addr(),
		"ipc_socket", d.IPCSocketPath(),
		"cert_fp", d.certFP.String(),
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.registry.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := d.quic.Serve(ctx); err != nil {
			errCh <- fmt.Errorf("quic serve: %w", err)
			cancel()
		}
	}()

	if d.loopbackTCP != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.loopbackTCP.Serve(ctx); err != nil {
				errCh <- fmt.Errorf("loopback tcp serve: %w", err)
				cancel()
			}
		}()
	}

	d.tcpMu.Lock()
	if d.tcp != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.tcp.Serve(ctx); err != nil {
				errCh <- fmt.Errorf("tcp serve: %w", err)
				cancel()
			}
		}()
	}
	d.tcpMu.Unlock()

	if d.deferredTCPSentinel != "" || d.tcpBoundIP != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.tailnetTCPPoller(ctx, &wg, errCh)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := d.ipc.Serve(ctx); err != nil {
			errCh <- fmt.Errorf("ipc serve: %w", err)
			cancel()
		}
	}()

	<-ctx.Done()
	_ = d.ipc.Close()
	_ = d.quic.Close()
	if d.loopbackTCP != nil {
		_ = d.loopbackTCP.Close()
	}
	d.tcpMu.Lock()
	if d.tcp != nil {
		_ = d.tcp.Close()
	}
	d.tcpMu.Unlock()
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

const tailnetPollInterval = 30 * time.Second

func (d *Daemon) tailnetTCPPoller(ctx context.Context, wg *sync.WaitGroup, errCh chan<- error) {
	port := ""
	if s := d.deferredTCPSentinel; s != "" {
		port = s[len("tailnet:"):]
	} else if d.tcpBoundIP != nil {
		d.tcpMu.Lock()
		if d.tcp != nil {
			port = fmt.Sprintf("%d", d.tcp.Addr().Port)
		}
		d.tcpMu.Unlock()
	}
	if port == "" {
		return
	}

	ticker := time.NewTicker(tailnetPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		ip, err := transport.ResolveTailnetBindIP()

		d.tcpMu.Lock()
		switch {
		case err != nil && d.tcp == nil:
			d.tcpMu.Unlock()
			d.logger.Debug("tailnet poller: no tailnet interface", "err", err)

		case err != nil && d.tcp != nil:
			d.logger.Warn("tailnet lost, disabling TCP listener",
				"was", d.tcpBoundIP)
			_ = d.tcp.Close()
			d.tcp = nil
			d.tcpBoundIP = nil
			d.deferredTCPSentinel = "tailnet:" + port
			d.tcpMu.Unlock()

		case err == nil && d.tcp == nil:
			var addr string
			if ip.To4() == nil {
				addr = fmt.Sprintf("[%s]:%s", ip.String(), port)
			} else {
				addr = fmt.Sprintf("%s:%s", ip.String(), port)
			}
			srv, srvErr := transport.NewTCPServer(transport.TCPConfig{
				Addr:     addr,
				StateDir: d.stateDir,
				Handler:  d.mtroamHandler,
				Logger:   d.logger,
				Limiter:  d.mtroamHandler.Limiter,
			})
			if srvErr != nil {
				d.tcpMu.Unlock()
				d.logger.Warn("tailnet detected but TCP bind failed",
					"addr", addr, "err", srvErr)
				continue
			}
			if srv == nil {
				d.tcpMu.Unlock()
				d.logger.Warn("tailnet detected but all TCP ports in range occupied")
				continue
			}
			d.tcp = srv
			d.tcpBoundIP = ip
			d.deferredTCPSentinel = ""
			d.tcpMu.Unlock()
			d.logger.Info("tailnet detected, TCP listener enabled",
				"addr", srv.Addr().String(), "ip", ip.String())
			wg.Add(1)
			go func() {
				defer wg.Done()
				if serveErr := srv.Serve(ctx); serveErr != nil {
					errCh <- fmt.Errorf("tcp serve (deferred): %w", serveErr)
				}
			}()

		case err == nil && d.tcp != nil && !d.tcpBoundIP.Equal(ip):
			d.logger.Info("tailnet IP changed, rebinding TCP",
				"old", d.tcpBoundIP, "new", ip)
			_ = d.tcp.Close()
			d.tcp = nil
			d.tcpBoundIP = nil
			d.tcpMu.Unlock()
			ticker.Reset(time.Second)
			continue

		default:
			d.tcpMu.Unlock()
		}
	}
}

// HandleAllocate is the IPC dispatch for AllocateRequest. It either
// looks up an existing session or creates a new one (spawning a
// PTY), then issues an attach token and returns the bootstrap line
// fields.
// Per-field size caps on IPC inputs. Same-uid trust model says the
// caller is friendly; these are defense-in-depth against a buggy or
// compromised local helper that could otherwise feed unbounded
// strings into our registry maps + log lines.
const (
	maxNameLen      = 256       // Session.Name; echoed in every list response + every log line.
	maxShellLen     = 4 * 1024  // Path to a shell binary; longer than this is pathological.
	maxExecJoinLen  = 16 * 1024 // Total bytes across all Exec[] joined by spaces.
	maxExecArgCount = 128       // Cap individual element count too — argv length is finite in practice.

	// maxSearchPatternLen caps the regex source length on SessionSearchRequest.
	// Go RE2 compile time is bounded but not free; a 1 KiB ceiling is well
	// above any human-typed pattern and rules out pathological compiles.
	maxSearchPatternLen = 1024
)

func (d *Daemon) HandleAllocate(ctx context.Context, req ipc.AllocateRequest) ipc.AllocateResponse {
	if msg := validateAllocateBounds(req); msg != "" {
		return ipc.AllocateResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: msg}
	}
	sess, reused, err := d.lookupOrCreateSession(req)
	if err != nil {
		// lookupOrCreateSession returns the response-shaped error
		// already; surface it.
		return errResponse(err)
	}
	// reused is already a *bool: non-nil for the core PTY paths, nil for
	// allocate paths that can't assert freshness (an AllocateExtension owns
	// its own spawn/reattach decision). A nil Reused omits the
	// MTRM_SESSION_REUSED line so clients fall back to their conservative
	// behavior.

	// hookInstalled mirrors reused's nil-for-extension posture: for a
	// core PTY spawn/reattach it is the session's stored live-inject
	// state (set at spawn / lazy respawn, round-tripped through
	// meta.cbor on a restored session); for an extension allocate it is
	// nil (the extension owns its own spawn, so the core must not assert
	// a hook state). reused != nil exactly distinguishes the core paths
	// from the extension path here.
	var hookInstalled *bool
	var shimReady *bool
	if reused != nil {
		hookInstalled = sess.HookInstalled()
		shimReady = sess.ShimReady()
	}

	tok, err := d.registry.IssueAttachToken(sess.ID())
	if err != nil {
		return ipc.AllocateResponse{Ok: false, Err: ipc.ErrInternal, Msg: err.Error()}
	}

	// Surface the optional mtRoam-over-TCP port to the iOS client.
	// 0 when --mtroam-tcp-addr wasn't supplied at startup; clients
	// in embedded-Tailscale mode then know to surface "host needs
	// daemon update" rather than try a dial that won't succeed.
	var tcpPort uint16
	d.tcpMu.Lock()
	if d.tcp != nil {
		tcpPort = uint16(d.tcp.Addr().Port)
	}
	d.tcpMu.Unlock()

	var loopbackPort uint16
	if d.loopbackTCP != nil {
		loopbackPort = uint16(d.loopbackTCP.Addr().Port)
	}

	return ipc.AllocateResponse{
		Ok:              true,
		SessionID:       sess.ID().String(),
		AttachToken:     tok.String(),
		Port:            uint16(d.quic.Addr().Port),
		TCPPort:         tcpPort,
		LoopbackTCPPort: loopbackPort,
		CertFP:          d.certFP.String(),
		Name:            sess.Name(),
		Reused:          reused,
		HookInstalled:   hookInstalled,
		ShimReady:       shimReady,
		BootID:          d.bootID,
	}
}

// HandlePing implements ipc.Handler.
func (d *Daemon) HandlePing(ctx context.Context, req ipc.PingRequest) ipc.PingResponse {
	return ipc.PingResponse{Nonce: req.Nonce}
}

// shimDirFor is the per-session PATH-shadow shim directory. Lives in the
// session's own state dir so it's cleaned up with the session and stays
// 0700/uid-private.
func (d *Daemon) shimDirFor(sid string) string {
	return sessionShimDir(d.stateDir, sid)
}

// HandleSetSessionSecrets stores a session's full secret set in memory
// (REPLACING any prior set) and (re)generates its PATH shims from the
// union of declared commands. Re-validates keys + commands here as the
// trust boundary. Requires the session to be live so a bogus id can't
// grow the store unboundedly. Values are held in RAM only — never
// written to the session snapshot.
func (d *Daemon) HandleSetSessionSecrets(_ context.Context, req ipc.SetSessionSecretsRequest) ipc.SetSessionSecretsResponse {
	sid, err := session.ParseSessionID(req.SessionID)
	if err != nil {
		return ipc.SetSessionSecretsResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: err.Error()}
	}
	if _, err := d.registry.Lookup(sid); err != nil {
		return ipc.SetSessionSecretsResponse{Ok: false, Err: ipc.ErrUnknownSession, Msg: err.Error()}
	}
	payload := secret.Payload{Secrets: make([]secret.Entry, 0, len(req.Secrets))}
	seen := make(map[string]struct{}, len(req.Secrets))
	for _, e := range req.Secrets {
		if !secret.ValidKey(e.Key) {
			return ipc.SetSessionSecretsResponse{Ok: false, Err: ipc.ErrBadRequest,
				Msg: "invalid key " + e.Key}
		}
		// Reject duplicates like ParsePayload does - this handler is the
		// trust boundary for DIRECT IPC callers too, and a silently
		// last-wins duplicate would deliver an unpredictable value
		// (review finding).
		if _, dup := seen[e.Key]; dup {
			return ipc.SetSessionSecretsResponse{Ok: false, Err: ipc.ErrBadRequest,
				Msg: "duplicate key " + e.Key}
		}
		seen[e.Key] = struct{}{}
		for _, c := range e.Cmds {
			if !secret.ValidCommand(c) {
				return ipc.SetSessionSecretsResponse{Ok: false, Err: ipc.ErrBadRequest,
					Msg: "invalid command " + c}
			}
		}
		payload.Secrets = append(payload.Secrets, secret.Entry{Key: e.Key, Value: e.Value, Cmds: e.Cmds})
	}
	sidStr := sid.String()
	d.shimSyncMu.Lock()
	defer d.shimSyncMu.Unlock()
	d.secrets.SetSession(sidStr, payload)
	cmds := d.secrets.Commands(sidStr)
	if err := secret.SyncShims(d.shimDirFor(sidStr), d.daemonBinary, d.IPCSocketPath(), cmds); err != nil {
		d.logger.Warn("secret.shims.sync_failed", "session", sidStr, "err", err.Error())
		return ipc.SetSessionSecretsResponse{Ok: false, Err: ipc.ErrInternal, Msg: err.Error()}
	}
	d.logger.Info("secret.set", "session", sidStr,
		"keys", len(payload.Secrets), "commands", len(cmds))
	return ipc.SetSessionSecretsResponse{Ok: true}
}

// HandleGetSecrets returns ONLY the env a command should be exec'd with
// in a session (least privilege). Empty Env (Ok=true) = nothing for that
// command; the caller then execs the real binary unchanged. Does not
// require the session to be live: a same-uid `secret-exec` querying a
// just-reaped session simply gets an empty result and fails open.
//
// The session id is CALLER-SUPPLIED (secret-exec reads it from the
// session env) and deliberately NOT bound to the requesting connection:
// the IPC socket is uid-private, and a same-uid process can already
// read any sibling session's /proc environ, state dir, or dial this
// socket directly - so per-session isolation against a same-uid caller
// is not enforceable at this layer, only at the OS-sandbox layer. The
// broker's promise (per the locked design) is keeping values out of
// the provider-bound agent CONTEXT and ambient env, not containing an
// adversarial same-uid process.
func (d *Daemon) HandleGetSecrets(_ context.Context, req ipc.GetSecretsRequest) ipc.GetSecretsResponse {
	return ipc.GetSecretsResponse{Ok: true, Env: d.secrets.EnvForCommand(req.SessionID, req.Command)}
}

// HandleListSessions returns a snapshot of every live session on
// the registry. The snapshot is taken in two passes — registry.IDs()
// under the registry's lock, then per-session Lookup + accessors —
// so a slow session reader (e.g. one whose mu is held by an Acquire
// in flight) can't stall the registry-wide enumeration. Best-effort:
// a session reaped between IDs() and Lookup is silently skipped.
func (d *Daemon) HandleListSessions(ctx context.Context, _ ipc.ListSessionsRequest) ipc.ListSessionsResponse {
	ids := d.registry.IDs()
	out := make([]ipc.SessionInfo, 0, len(ids))
	for _, id := range ids {
		sess, err := d.registry.Lookup(id)
		if err != nil {
			continue
		}
		rows, cols := sess.WindowSize()
		modes := sess.AttachedModes()
		totalOut, resizes, silent, cursor, vwalk := sess.WedgeSnapshot()
		out = append(out, ipc.SessionInfo{
			ID:                      sess.ID().String(),
			Name:                    sess.Name(),
			CreatedAtNs:             sess.Created().UnixNano(),
			LastActiveAtNs:          sess.LastActiveAt().UnixNano(),
			AttachedNow:             len(modes) > 0,
			AttachedModes:           modes,
			IdleTimeoutNs:           int64(sess.IdleTimeout()),
			Rows:                    rows,
			Cols:                    cols,
			Fg:                      sess.ForegroundComm(),
			Labels:                  sess.Labels(),
			WedgeTotalOutBytes:      totalOut,
			WedgeResizesObserved:    resizes,
			WedgeSilentWedges:       silent,
			WedgeCursorWedges:       cursor,
			WedgeVerticalWalkWedges: vwalk,
		})
	}
	return ipc.ListSessionsResponse{Ok: true, Sessions: out}
}

// HandleStatus returns the daemon's operational snapshot. Pure
// read — no side effects. Used by `mtroamd status`, by Phase 5's
// install-flow version probe, and by systemd-unit health checks.
func (d *Daemon) HandleStatus(ctx context.Context, _ ipc.StatusRequest) ipc.StatusResponse {
	now := time.Now()
	return ipc.StatusResponse{
		Ok:               true,
		Version:          build.String(),
		StartedAtNs:      d.startedAt.UnixNano(),
		UptimeNs:         now.Sub(d.startedAt).Nanoseconds(),
		QUICAddr:         d.quic.Addr().String(),
		MTRoamTCPAddr:    d.TCPAddr(),
		CertFingerprint:  d.certFP.String(),
		SessionCount:     d.registry.Len(),
		MaxSessions:      d.registry.Capacity(),
		IdleTimeoutNs:    int64(d.registry.IdleTimeout()),
		MaxIdleTimeoutNs: int64(d.registry.MaxIdleTimeout()),
		PendingTokens:    d.registry.PendingTokenCount(),
	}
}

// HandleRenameSession changes a session's user-visible Name.
// Selector resolution mirrors KillSession: hex SessionID first,
// fall back to LookupByName. The PTY + ring buffer + active
// attach are unaffected — this is a pure-label change.
func (d *Daemon) HandleRenameSession(ctx context.Context, req ipc.RenameSessionRequest) ipc.RenameSessionResponse {
	if req.Sel == "" {
		return ipc.RenameSessionResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: "selector required"}
	}
	if req.NewName == "" {
		return ipc.RenameSessionResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: "new name required"}
	}
	if len(req.NewName) > maxNameLen {
		return ipc.RenameSessionResponse{Ok: false, Err: ipc.ErrBadRequest,
			Msg: fmt.Sprintf("new name exceeds %d bytes", maxNameLen)}
	}

	// Resolve selector → SessionID.
	var sid session.SessionID
	if parsed, err := session.ParseSessionID(req.Sel); err == nil {
		sid = parsed
	} else {
		sess, lerr := d.registry.LookupByName(req.Sel)
		if lerr != nil {
			return ipc.RenameSessionResponse{Ok: false, Err: ipc.ErrUnknownSession, Msg: lerr.Error()}
		}
		sid = sess.ID()
	}

	if err := d.registry.Rename(sid, req.NewName); err != nil {
		code := ipc.ErrInternal
		switch {
		case errors.Is(err, session.ErrUnknownSession):
			code = ipc.ErrUnknownSession
		case errors.Is(err, session.ErrDuplicateName):
			code = ipc.ErrNameInUse
		}
		return ipc.RenameSessionResponse{Ok: false, Err: code, Msg: err.Error()}
	}
	d.logger.Info("session renamed",
		"session", sid.String(),
		"name_hash", session.NameHash(sid, req.NewName),
	)
	d.logger.Debug("session.renamed.name",
		"session", sid.String(),
		"name", req.NewName,
	)
	return ipc.RenameSessionResponse{Ok: true, Name: req.NewName}
}

// HandleKillSession reaps a session by hex SessionID or by Name.
// Selector resolution: try parse as hex SessionID first; on parse
// failure, try LookupByName. Either way, the registry's Remove
// closes the session (which terminates the PTY and cancels any
// active attach).
func (d *Daemon) HandleKillSession(ctx context.Context, req ipc.KillSessionRequest) ipc.KillSessionResponse {
	if req.Sel == "" {
		return ipc.KillSessionResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: "selector required"}
	}
	if sid, err := session.ParseSessionID(req.Sel); err == nil {
		// Selector parsed as a SessionID — verify the session exists
		// before reporting success, so the caller can distinguish
		// "I asked you to kill X" from "X was already gone."
		if _, lerr := d.registry.Lookup(sid); lerr != nil {
			return ipc.KillSessionResponse{Ok: false, Err: ipc.ErrUnknownSession, Msg: lerr.Error()}
		}
		d.registry.Remove(sid)
		d.logger.Info("session killed", "session", sid.String(), "by", "id")
		return ipc.KillSessionResponse{Ok: true}
	}
	// Fall through: treat as a name.
	sess, err := d.registry.LookupByName(req.Sel)
	if err != nil {
		return ipc.KillSessionResponse{Ok: false, Err: ipc.ErrUnknownSession, Msg: err.Error()}
	}
	id := sess.ID()
	d.registry.Remove(id)
	d.logger.Info("session killed",
		"session", id.String(),
		"name_hash", session.NameHash(id, req.Sel),
		"by", "name",
	)
	d.logger.Debug("session.killed.name",
		"session", id.String(),
		"name", req.Sel,
	)
	return ipc.KillSessionResponse{Ok: true}
}

// HandleSessionSearch scans the named session's scrollback ring for
// regex matches. Selector resolution mirrors HandleKillSession: parse
// as hex SessionID first, then fall back to LookupByName.
//
// The regex is compiled here (not on the wire) so the daemon enforces
// the size cap + RE2 semantics. Anchored=true wraps the pattern in a
// (?m:…) non-capturing group so ^/$ match physical newlines without
// disturbing any flags the caller embedded.
func (d *Daemon) HandleSessionSearch(_ context.Context, req ipc.SessionSearchRequest) ipc.SessionSearchResponse {
	if req.Sel == "" {
		return ipc.SessionSearchResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: "selector required"}
	}
	if req.Pattern == "" {
		return ipc.SessionSearchResponse{Ok: false, Err: ipc.ErrBadRequest, Msg: "pattern required"}
	}
	if len(req.Pattern) > maxSearchPatternLen {
		return ipc.SessionSearchResponse{Ok: false, Err: ipc.ErrBadRequest,
			Msg: fmt.Sprintf("pattern exceeds %d bytes", maxSearchPatternLen)}
	}

	src := req.Pattern
	if req.Anchored {
		src = "(?m:" + src + ")"
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return ipc.SessionSearchResponse{Ok: false, Err: ipc.ErrBadRequest,
			Msg: "compile pattern: " + err.Error()}
	}

	sess, err := d.resolveSessionBySelector(req.Sel)
	if err != nil {
		return ipc.SessionSearchResponse{Ok: false, Err: ipc.ErrUnknownSession, Msg: err.Error()}
	}

	hits := sess.Buffer().Search(re, session.SearchOpts{MaxMatches: req.MaxMatches})
	infos := make([]ipc.SearchMatchInfo, len(hits))
	for i, h := range hits {
		infos[i] = ipc.SearchMatchInfo{
			StartSeq: h.StartSeq,
			EndSeq:   h.EndSeq,
			Line:     h.Line,
			LineNum:  h.LineNum,
		}
	}
	return ipc.SessionSearchResponse{Ok: true, Matches: infos}
}

// resolveSessionBySelector returns the session named by sel, applying
// the id-or-name fallback used by Kill, Rename, and SessionSearch.
// Tries hex SessionID parse first; on parse failure, looks up by name.
func (d *Daemon) resolveSessionBySelector(sel string) (*session.Session, error) {
	if sid, err := session.ParseSessionID(sel); err == nil {
		return d.registry.Lookup(sid)
	}
	return d.registry.LookupByName(sel)
}

// lookupOrCreateSession returns the session referenced by req. The
// resolution rules cover four cases:
//
//  1. SessionID is hex (parses cleanly) → look up by ID; error
//     if missing. Reattach path; req.Name is ignored.
//  2. SessionID == "" || SessionID == "new", req.Name is set →
//     "create-if-missing": look up by name; if found, attach to
//     it. If not found, spawn a fresh session with that name.
//     Matches tmux's `new -A -s name` idiom and is what the iOS
//     picker uses for "+ New named X" + every plain-tap probe.
//  3. SessionID == "" || SessionID == "new", req.Name is empty →
//     spawn a fresh anonymous session (daemon synthesises name).
//     Legacy "any session, no preferences" allocate.
//  4. SessionID is neither "new" nor a parseable hex string →
//     ErrBadRequest. Future overload (e.g., "name:foo") would land
//     here, but we don't ship that — see B1 in the plan.
//
// Errors are wrapped in allocateErr so the caller can map them to
// AllocateResponse fields.
// lookupOrCreateSession resolves an allocate to a session. `reused`
// reports which way it went: non-nil true when the request landed on an
// already-running session (by-id lookup, or create-by-name that found
// one), non-nil false for a fresh core PTY spawn. It is NIL (unknown)
// for an extension allocate: an AllocateExtension owns its own
// spawn-vs-reattach decision, which the core cannot observe, so it must
// not assert freshness on the extension's behalf (a downstream agent
// fork that resumes a running session would otherwise be mis-reported
// as fresh). The value feeds AllocateResponse.Reused; nil omits the
// MTRM_SESSION_REUSED line and clients fall back conservatively.
func (d *Daemon) lookupOrCreateSession(req ipc.AllocateRequest) (sess *session.Session, reused *bool, err error) {
	reusedTrue, reusedFalse := true, false
	if req.SessionID == "" || req.SessionID == "new" {
		// Extension-handled allocate: a non-empty Kind routes to a
		// registered AllocateExtension (downstream embedder). The core
		// registers none, so a stock terminal daemon rejects any Kind.
		if req.Kind != "" {
			ext, ok := d.allocateExts[req.Kind]
			if !ok {
				return nil, nil, &allocateErr{Code: ipc.ErrBadRequest, Msg: "unknown allocate kind: " + req.Kind}
			}
			sess, err = ext.Spawn(context.Background(), d.spawnEnv(), req)
			return sess, nil, err
		}
		// Name-driven path: prefer reattach to an existing session
		// with this name, fall back to spawn. Empty Name → plain
		// anonymous spawn (legacy).
		if req.Name != "" {
			if sess, err := d.registry.LookupByName(req.Name); err == nil {
				d.applyIdleTimeoutOnReattach(sess, req)
				return sess, &reusedTrue, nil
			}
			// Not found — fall through to spawn (which will use
			// req.Name and may collide if the name was added
			// concurrently between LookupByName and Add; in that
			// race the spawn returns ErrNameInUse, which is
			// surfaced verbatim).
		}
		sess, err = d.spawnSession(req)
		return sess, &reusedFalse, err
	}

	sid, err := session.ParseSessionID(req.SessionID)
	if err != nil {
		return nil, nil, &allocateErr{Code: ipc.ErrBadRequest, Msg: err.Error()}
	}
	sess, err = d.registry.Lookup(sid)
	if err != nil {
		return nil, nil, &allocateErr{Code: ipc.ErrUnknownSession, Msg: err.Error()}
	}
	// Do NOT resize the PTY on reattach. iOS sends a Resize control
	// frame after Attach with the actual terminal size; if we also
	// resize here from req.Rows/Cols, the PTY size bounces (e.g.
	// 40×45 → 24×80 from the hardcoded CLI args, then 40×45 again
	// from the QUIC Resize) and each transition fires SIGWINCH at
	// the child shell, which redraws its prompt — two extra prompt
	// bytes go into the ring buffer per cold-start.
	//
	// req.Rows/Cols are still meaningful for the spawn path above
	// (initial PTY size for new sessions). For reattach, the QUIC
	// control-frame path is the single source of truth.
	d.applyIdleTimeoutOnReattach(sess, req)
	return sess, &reusedTrue, nil
}

// applyIdleTimeoutOnReattach updates an existing session's idle
// timeout when the client's request specifies a different value.
// Without this the session would keep its original timeout — the
// iOS-side Keep-alive picker would silently no-op on reattach,
// which historically caused sessions to be GC'd at the OLD interval
// after a user edited the host (e.g. 1h → 30d).
//
// req.IdleTimeoutNanos == 0 means "use the daemon default" and is
// applied verbatim (matches the spawn-time semantics). A non-zero
// value is clamped at the registry's --max-idle-timeout ceiling
// before being written so an operator-set bound isn't bypassed.
func (d *Daemon) applyIdleTimeoutOnReattach(sess *session.Session, req ipc.AllocateRequest) {
	resolved := d.registry.ResolveIdleTimeout(time.Duration(req.IdleTimeoutNanos))
	if resolved == sess.IdleTimeout() {
		return
	}
	prev := sess.IdleTimeout()
	sess.SetIdleTimeout(resolved)
	d.logger.Info("session.idle_timeout_updated",
		"session", sess.ID().String(),
		"name_hash", session.NameHash(sess.ID(), sess.Name()),
		"prev", prev.String(),
		"new", resolved.String(),
	)
	d.logger.Debug("session.idle_timeout_updated.name",
		"session", sess.ID().String(),
		"name", sess.Name(),
	)
}

// spawnSession opens a PTY + child shell, wraps it in a Session,
// adds to the registry, and starts the Pump.
func (d *Daemon) spawnSession(req ipc.AllocateRequest) (*session.Session, error) {
	sid, err := session.NewSessionID()
	if err != nil {
		return nil, &allocateErr{Code: ipc.ErrInternal, Msg: "generate session id: " + err.Error()}
	}

	rows, cols := req.Rows, req.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	// Default-naming policy: every session has a non-empty
	// user-visible name, even when the client didn't supply one.
	// `session-<first-6-hex-of-id>` is short enough to fit a chip,
	// stable across reattaches, and impossible to collide with a
	// user-chosen name (no user picks 6 hex chars deliberately).
	name := req.Name
	if name == "" {
		name = "session-" + sid.String()[:6]
	}

	// Resolve the per-session idle timeout: client request → ceiling
	// → daemon default. Stored on the Session itself so future GC
	// sweeps consult its value rather than the daemon-wide default.
	idleTimeout := d.registry.ResolveIdleTimeout(time.Duration(req.IdleTimeoutNanos))

	// Spawn the PTY-owning sidecar process. The sidecar holds the
	// child shell as a direct subprocess and survives subsequent
	// daemon restarts; the returned *ptyclient.Conn implements
	// session.PTY and slots in everywhere a *pty.Handle used to.
	//
	// MESHTERM_ROAM=1 lets user shells short-circuit auto-tmux blocks
	// in their rc files (we don't want mtRoam shells to nest inside the
	// user's regular tmux session — see the recommended guard form
	// in the sidecarExtraEnv comment). The mtRoam shell already
	// persists via mtroamd's own session machinery, so skipping
	// tmux is a no-op from the user's persistence perspective.
	// User-supplied env (from `mtroamd connect --env-file`) merges AFTER
	// the daemon's curated vars, so a client key can override only on a
	// deliberate collision (never happens for the MESHTERM_* names). Keys
	// are sorted for a deterministic env-file ordering.
	extraEnv := sessionExtraEnvForID(sid)
	if len(req.Env) > 0 {
		keys := make([]string, 0, len(req.Env))
		for k := range req.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			extraEnv = append(extraEnv, k+"="+req.Env[k])
		}
	}
	// Shim env LAST, prepending the shim dir to the user's PATH (req.Env
	// PATH if set, else the daemon's) - so a client's custom PATH survives.
	extraEnv = append(extraEnv, shimSpawnEnv(d.stateDir, sid.String(), req.Env["PATH"])...)
	ptyHandle, err := ptyclient.SpawnNew(context.Background(), ptyclient.SpawnConfig{
		SessionID:    sid.String(),
		Shell:        req.Shell,
		ShellArgs:    req.Exec,
		Rows:         rows,
		Cols:         cols,
		ExtraEnv:     extraEnv,
		StateDir:     d.stateDir,
		DaemonBinary: d.daemonBinary,
		Logger:       d.logger,
		Stderr:       d.cfg.SidecarStderr,
	})
	if err != nil {
		return nil, &allocateErr{Code: ipc.ErrSpawnFailed, Msg: err.Error()}
	}

	// Operator-configurable buffer cap; 0 falls back to
	// session.DefaultBufferCapacity inside NewSession.
	sess, err := session.NewSession(sid, name, ptyHandle, rows, cols, d.cfg.SessionBufferBytes, idleTimeout)
	if err != nil {
		_ = ptyHandle.Close()
		return nil, &allocateErr{Code: ipc.ErrInternal, Msg: err.Error()}
	}
	// Wire the wedge-watcher's JSONL output to the daemon's state dir.
	// File is created lazily by the watcher (O_APPEND|O_CREATE) on
	// the first detected wedge.
	if d.stateDir != "" {
		sess.SetWedgeLogPath(filepath.Join(d.stateDir, WedgeReportFilename))
	}

	// Resolve persistence tri-state. nil → daemon default
	// (`--persistence-default`, default-on). Wire the flag before
	// the session enters the registry so a Sweep or Remove that
	// fires before we start the flusher already sees the correct
	// persist value.
	persist := d.registry.ResolvePersist(req.Persist)
	sess.SetPersist(persist)

	// Record the sidecar's live-inject hook state on the session so it
	// is persisted (meta.cbor) and surfaced on this + future allocates
	// via AllocateResponse.HookInstalled. ptyHandle is the *ptyclient.Conn
	// that carries the value the detached sidecar reported.
	sess.SetHookInstalled(ptyHandle.HookInstalled())
	// shimReady is the sidecar-verified value (it re-asserts the shim dir
	// on PATH after the user's rc and reports whether it is guaranteed),
	// not an assumption - persisted + surfaced on AllocateResponse.
	sess.SetShimReady(ptyHandle.ShimReady())

	if err := d.registry.Add(sess); err != nil {
		_ = sess.Close()
		code := ipc.ErrInternal
		switch {
		case errors.Is(err, session.ErrCapacityReached):
			code = ipc.ErrCapacity
		case errors.Is(err, session.ErrDuplicateName):
			code = ipc.ErrNameInUse
		}
		return nil, &allocateErr{Code: code, Msg: err.Error()}
	}

	// Start the persistence flusher (no-op when persist is false).
	// Lifecycle is owned by the Session — Session.Close stops the
	// goroutine and does one final write.
	if persist {
		sess.StartFlusher(d.stateDir, d.cfg.PersistenceFlushInterval, d.logger)
	}

	go sess.Pump()
	d.logger.Info("session spawned",
		"session", sid.String(),
		"name_hash", session.NameHash(sid, name),
		"rows", rows, "cols", cols,
		"persist", persist,
	)
	d.logger.Debug("session.spawned.name",
		"session", sid.String(),
		"name", name,
	)
	return sess, nil
}

// allocateErr is a typed error carrying the wire-level error code +
// message that should appear in AllocateResponse.
type allocateErr struct {
	Code string
	Msg  string
}

func (e *allocateErr) Error() string { return e.Code + ": " + e.Msg }

func errResponse(err error) ipc.AllocateResponse {
	var ae *allocateErr
	if errors.As(err, &ae) {
		return ipc.AllocateResponse{Ok: false, Err: ae.Code, Msg: ae.Msg}
	}
	return ipc.AllocateResponse{Ok: false, Err: ipc.ErrInternal, Msg: err.Error()}
}

// validateAllocateBounds checks the request's string fields against
// the per-field size caps. Returns "" if all good, otherwise a
// caller-facing error message.
//
// CBOR's StrictDecMode already bounds total frame size + array/map
// fanout, but a single string field can still consume the full
// per-frame budget. These caps are the inner second line of defence.
func validateAllocateBounds(req ipc.AllocateRequest) string {
	if len(req.Name) > maxNameLen {
		return fmt.Sprintf("name exceeds %d bytes", maxNameLen)
	}
	if len(req.Shell) > maxShellLen {
		return fmt.Sprintf("shell path exceeds %d bytes", maxShellLen)
	}
	if len(req.Exec) > maxExecArgCount {
		return fmt.Sprintf("exec has %d args; max is %d", len(req.Exec), maxExecArgCount)
	}
	joined := 0
	for _, a := range req.Exec {
		joined += len(a) + 1 // +1 approximates a separator
		if joined > maxExecJoinLen {
			return fmt.Sprintf("exec args total exceed %d bytes", maxExecJoinLen)
		}
	}
	return ""
}
