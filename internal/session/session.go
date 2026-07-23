package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/altscreen"
)

// SessionIDLen is the byte length of a session identifier.
const SessionIDLen = 16

// AttachMode is the role a client takes when attaching. The wire
// representation is a lowercase string on the Attach control frame
// (see `protocol.AttachModeExclusive` / `AttachModeReadonly`); we
// keep an internal Go-typed mirror so the session code doesn't
// depend on the protocol package directly.
type AttachMode int

const (
	// AttachExclusive is the default attach mode. The client
	// receives output, sends stdin, and owns the PTY size via
	// Resize. A new exclusive attach displaces any prior exclusive
	// attach (existing readonly attaches are unaffected). This is
	// the only mode pre-multi-attach clients can request, and the
	// only mode where stdin actually reaches the shell.
	AttachExclusive AttachMode = iota

	// AttachReadonly is the watcher mode. Receives output, doesn't
	// send stdin or Resize (the daemon drops them on the protocol
	// boundary so a misbehaving keystroke can't tear the
	// connection down). Any number of readonly clients can coexist
	// with each other and with a single exclusive client.
	AttachReadonly

	// AttachPassive is the invisible-tap mode. Like readonly —
	// receives output, stdin/resize frames dropped — but invisible
	// to AttachedModes / PeerModes. Capped at MaxPassivePerSession.
	// See protocol.AttachModePassive for the wire-form rationale.
	AttachPassive

	// AttachExclusiveIfFree is a REQUEST-only mode: Acquire resolves
	// it to AttachExclusive (no live exclusive client) or
	// AttachReadonly (someone already holds exclusive) atomically
	// under the session lock, and the resolved mode is what gets
	// stored and returned. A sessionClient never carries this value.
	// See protocol.AttachModeExclusiveIfFree.
	AttachExclusiveIfFree
)

// MaxPassivePerSession caps the number of concurrent passive
// attachers per session. Resource defence; passive attaches are
// cheap (one goroutine per stream, no PTY ownership) but unbounded
// passive multi-attach would burn fds + goroutines on a runaway
// `mtroam tail` loop.
const MaxPassivePerSession = 8

// MaxReadonlyPerSession caps concurrent read-only attachers per
// session, mirroring MaxPassivePerSession. Read-only attaches are
// token-gated (only the SSH-authenticated control path can mint an
// attach token), so this is not an unauthenticated-DoS bound — it
// caps the amplification a compromised or buggy authenticated client
// can drive (each attach spawns ~6 goroutines + a ring waiter).
// Generous relative to passive since read-only is a deliberate
// multi-viewer mode.
const MaxReadonlyPerSession = 16

// String returns the wire form of an AttachMode for logging /
// AttachAck.Mode echo. Mirrors protocol.AttachMode* constants.
func (m AttachMode) String() string {
	switch m {
	case AttachReadonly:
		return "readonly"
	case AttachPassive:
		return "passive"
	default:
		return "exclusive"
	}
}

// sessionClient is the per-attach state stored inside a Session
// while a client is connected. The cancel func is the goroutine-
// local context-cancellation hook the daemon uses to evict a
// client (e.g. when an exclusive replacement displaces the prior
// exclusive). gen is the monotonic identity used by Release to
// distinguish "this is me, removing myself" from "I was already
// kicked out" — see the activeGen rationale on Session.
type sessionClient struct {
	gen    uint64
	mode   AttachMode
	cancel context.CancelFunc
	// clientID is the attach's stable per-device id (protocol.Attach.ClientID),
	// "" when the client sent none. Used so an exclusive-if-free attach from the
	// SAME client silently displaces its own stale connection instead of landing
	// readonly. See acquireLocked.
	clientID string
	// notifyReplaced, when non-nil, is invoked (best-effort, once)
	// just before this client's context is cancelled because a new
	// exclusive attach displaced it. The transport layer installs it
	// via SetReplacedNotifier to push Goodbye{reason:"replaced"} so
	// the client can tell displacement from a network drop. Must be
	// safe to call from another goroutine and must not block.
	notifyReplaced func()
}

// SessionID is a 16-byte random identifier for a Session, generated at
// session creation. The ID confers no authority on its own — see
// docs/SECURITY.md threat E.
type SessionID [SessionIDLen]byte

// NewSessionID returns a fresh random session ID using crypto/rand.
func NewSessionID() (SessionID, error) {
	var id SessionID
	_, err := rand.Read(id[:])
	return id, err
}

// String returns the hex encoding (32 chars) used in the bootstrap line
// and in client-facing diagnostics.
func (id SessionID) String() string {
	return hex.EncodeToString(id[:])
}

// Bytes returns a fresh copy of the session ID's raw bytes for
// inclusion in CBOR-encoded protocol messages where the wire form
// is `bytes` not `string`.
func (id SessionID) Bytes() []byte {
	out := make([]byte, len(id))
	copy(out, id[:])
	return out
}

// ParseSessionID parses a 32-char hex SessionID. Returns an error on
// any deviation from that format.
func ParseSessionID(s string) (SessionID, error) {
	var id SessionID
	if len(s) != SessionIDLen*2 {
		return id, errors.New("session id must be 32 hex chars")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}

// PTY is the abstraction the Session needs from a pseudo-terminal.
// Real implementations wrap creack/pty; tests can substitute pipes.
//
// Read returns bytes from the PTY's slave-side output (what the user
// sees on screen). Write sends bytes to the PTY's stdin (keyboard
// input). SetSize forwards a window-size change. Close terminates
// the PTY and releases the file descriptors.
type PTY interface {
	io.Reader
	io.Writer
	io.Closer
	SetSize(rows, cols uint16) error
}

// SeqAwarePTY is implemented by sidecar-backed PTY connections that
// expose the underlying byte-seq counter. Pump probes for this
// interface so it can ack consumed bytes back to the sidecar (freeing
// them from the sidecar's drop-oldest ring) and propagate Trunc
// events as RingBuffer.AdvanceWithGap calls. In-process PTYs
// (internal/pty.Handle) don't implement this — the legacy path
// behaves exactly as before.
type SeqAwarePTY interface {
	// ConsumeTrunc returns the byte count silently dropped since the
	// last call, resetting the counter. Pump applies this via
	// RingBuffer.AdvanceWithGap before its next Write so the daemon-
	// ring's headSeq jumps past the lost span.
	ConsumeTrunc() uint64

	// LastConsumedSeq returns the sidecar-side seq just past the
	// last byte the daemon has read off the wire (= what Pump should
	// pass to AdvanceSidecarSeq + Ack after a successful buf.Write).
	LastConsumedSeq() uint64

	// Ack tells the sidecar bytes ≤ consumedSeq can be freed.
	Ack(consumedSeq uint64) error
}

// Session is one persistent terminal: a PTY + child process + an
// output ring buffer. Sessions outlive QUIC connections; clients
// attach by ID, the buffer replays anything they missed.
type Session struct {
	id      SessionID
	name    string
	created time.Time
	cap     int

	mu sync.Mutex

	buf  *RingBuffer
	pty  PTY
	rows uint16
	cols uint16

	// idleTimeout is how long this session may sit idle (no PTY
	// output, no stdin, no resize, no attach) before the registry's
	// GC reaps it. Set per-session at creation time so different
	// hosts/sessions can carry different lifetimes — a long-lived
	// dev box wants 7 days, a one-off CI shell can stay at the
	// daemon's hour default. Zero means "use the registry's
	// default", per the constructor's contract.
	idleTimeout time.Duration

	// Last time something happened on this session (PTY output or
	// active attach). Drives the registry's idle-GC.
	lastActiveAt time.Time

	// clients is the set of currently-attached clients. There is at
	// most one client whose mode is exclusive; any number whose
	// mode is readonly may coexist with it AND with each other.
	// Pre-multi-attach this was a single (cancel, gen) slot; the
	// slice form generalises to read-only watchers (Tier 1
	// shared-attach) and tmux-style co-equal pair-programming
	// (Tier 2 — protocol headroom is reserved, semantics deferred).
	clients []sessionClient

	// passiveClients is the parallel slice for AttachPassive attachers.
	// Kept separate so AttachedModes / PeerModes (which iterate only
	// `clients`) automatically hide passive watchers — no per-call
	// filtering needed. Capped at MaxPassivePerSession. Genshare the
	// same nextGen counter so Release(gen) can be a single linear
	// scan across both slices.
	passiveClients []sessionClient

	// nextGen monotonically counts attach calls. Each successful
	// Acquire returns a fresh value; Release(gen) only removes the
	// matching client. Audit F4 (v0.0.2 review) — this replaces
	// the ctx-error-as-identity heuristic with a proper monotonic
	// generation counter so a displaced client calling Release
	// after the new owner has taken over does NOT stomp the new
	// owner's state.
	nextGen uint64

	// persist indicates whether this session should be snapshotted
	// to disk so it survives daemon restart. Set at spawn time from
	// the IPC AllocateRequest's Persist field (clamped through
	// Registry.ResolvePersist so the daemon-wide default applies
	// when the client didn't specify). Persisted sessions get a
	// background flusher goroutine started by the registry.
	persist bool

	// lastSnapshotSeq is the buffer's headSeq at the last successful
	// disk snapshot. The flusher compares the buffer's current
	// headSeq to this value; if equal, nothing new has arrived and
	// the snapshot is skipped. Updated only after a successful
	// write (so a failed flush leaves dirty state and triggers
	// another attempt on the next tick).
	lastSnapshotSeq uint64

	// lastSidecarSeq is the highest sidecar-side outSeq the daemon
	// has durably committed to this session's ring. Persisted in
	// meta.cbor; consulted by the discovery path to compute the
	// FrameResume(from_seq) the daemon sends to a reattached sidecar.
	// Advanced by Pump on every successful buf.Write — protected by
	// s.mu.
	lastSidecarSeq uint64

	// hookInstalled records whether the sidecar seeded a working
	// prompt hook (the live-inject shim) into this session's shell at
	// spawn. Tri-state via pointer: nil = unknown (never spawned by a
	// hook-aware sidecar, or an old sidecar that didn't report), *true
	// = a bash/zsh prompt hook is installed and will fire, *false = no
	// working hook (dash/sh, an unknown shell, or a seeding failure).
	// Set by the daemon from the ptyclient spawn result at spawn and
	// on lazy respawn; persisted in meta.cbor so a reattach after a
	// daemon restart reports the stored value. HandleAllocate surfaces
	// it as AllocateResponse.HookInstalled → the MTRM_LIVE_INJECT
	// bootstrap line. Guarded by s.mu.
	hookInstalled *bool

	// shimReady records whether this session's shell was spawned with
	// the secret-broker PATH-shadow shim dir on its PATH (shimSpawnEnv,
	// present since v1.7.8-rc1). Tri-state via pointer: nil = unknown
	// (spawned by a pre-broker daemon, or never reported) → a brokered
	// secret would NOT reach this shell's tools until it is regenerated;
	// *true = the shim dir is on PATH so `set-secrets` reaches the tools.
	// Set true at spawn + lazy respawn (both call shimSpawnEnv); persisted
	// in meta.cbor so a reattach after a daemon restart reports the stored
	// value. HandleAllocate surfaces it as AllocateResponse.ShimReady →
	// the MTRM_SHIM_READY bootstrap line, so iOS can warn "regenerate this
	// session for hidden secrets to take effect". Guarded by s.mu.
	shimReady *bool

	// restoredFromDisk is true when this Session was hydrated from
	// on-disk state at daemon startup (LoadPersisted), rather than
	// freshly spawned. Cleared the first time a client successfully
	// attaches — until then the AttachAck.Restored flag fires so
	// the client can surface a "Restored from previous session"
	// banner. After first attach the session behaves identically
	// to a freshly-spawned one.
	restoredFromDisk bool

	// firstAttachPending is true until the first AttachAck with OK=true
	// has been sent for this session. NewSession sets it; LoadPersisted
	// explicitly clears it (a restored session has, by definition, been
	// attached to before). The protocol_handler reads it via
	// ConsumeFirstAttach and clears it atomically right before sending
	// AttachAck so clients see FreshlyCreated=true on exactly one
	// attach — the very first.
	firstAttachPending bool

	// flusherCancel and flusherDone are the lifecycle handles for
	// the background snapshot goroutine. Non-nil while running.
	// Cleared by stopFlusher (called from Close). Idempotent —
	// StartFlusher checks for non-nil and no-ops.
	flusherCancel context.CancelFunc
	flusherDone   chan struct{}

	// wedge runs the resize-wedge detector. Non-nil after NewSession;
	// the daemon wires its JSONL log path via SetWedgeLogPath after
	// stateDir is resolved. The watcher updates totalOutBytes from
	// Pump and arms a deadline timer from Resize; see wedgewatch.go.
	wedge *wedgeWatcher

	// titleTracker watches the PTY byte stream for OSC 0/2 title-
	// setting sequences. Non-nil after NewSession. Persists to
	// meta.cbor via persistedSessionMeta.LastTitle so a client that
	// reattaches after the original OSC is evicted from the ring
	// still sees the right title via AttachAck.LastTitle and can
	// prime its terminal emulator's title before replay. Mirrors the
	// alt-screen pattern from v1.1.2/v1.1.3.
	titleTracker *oscTitleTracker

	// screen is a live VT screen-model of the terminal, fed the same
	// post-QueryFilter byte stream as the ring in Pump. It lets an
	// alt-screen (full-screen TUI) attach replay a synthesized CLEAN
	// full-frame redraw (AltScreenRepaint) instead of a mid-stream
	// truncated byte tail the client can't reassemble into a 2-D screen
	// — the root cause of cold-start alt-screen spill. Guarded by
	// screenMu (Pump is the writer; the attach path reads via Repaint).
	// When the model is unsure (unfaithful ops, or a sidecar gap marked
	// it stale) the attach path falls back to raw replay, never worse
	// than before. Non-nil after NewSession.
	screen   *altscreen.Screen
	screenMu sync.Mutex

	// ptyByteObserver, if set, receives every chunk Pump reads from
	// the PTY (post-QueryFilter, same bytes the client renders).
	// Installed by the recovery sequencer to detect bookend markers
	// Claude prints during the save phase ("Commencing Save…" /
	// "Memory Updated, restoring…"). Cleared by the sequencer on
	// exit so a future recovery starts fresh. The observer runs in
	// the Pump goroutine; callers must keep it non-blocking and
	// thread-safe. Identity-keyed by the recover generation (M4): a
	// superseded recover's deferred clear must only clear the slot if it
	// still owns it, never the live recover's observer.
	ptyByteObserver    func([]byte)
	ptyByteObserverGen uint64

	// Foreground transition anchors (v1.6.3+), derived by
	// observeForegroundAnchor from the PTY's foreground command. The
	// seq anchor is in the ring's post-filter byte-sequence space
	// (buf.HeadSeq) — the same space the client tracks — so a client's
	// size = currentSeq-fgAnchorSeq is well defined. fgAnchorInit
	// distinguishes "never observed" from a genuine "" foreground.
	// Own mutex so the hot Pump observe path never contends s.mu.
	fgAnchorMu   sync.Mutex
	fgAnchorComm string
	fgAnchorTime time.Time
	fgAnchorSeq  uint64
	fgAnchorInit bool

	// recoverCancel cancels the in-flight recovery goroutine. Non-nil
	// while a recovery is running. Protected by mu. recoverGen
	// identifies the recovery that currently owns the slot so a
	// finishing goroutine only frees it when it still owns it — see
	// ClearRecover.
	recoverCancel context.CancelFunc
	recoverGen    uint64

	// Ext is a generic per-session extension slot for an embedding binary
	// to attach its own state to a session. The core never reads or
	// interprets it — a terminal-only daemon leaves it nil. (Used by
	// downstream embedders to hang per-session state off a session without
	// the core needing to know what that state is.)
	Ext any

	// --- stream backend (see stream.go) ---
	// A session is either PTY-backed (pty != nil; output via Pump→buf) OR
	// stream-backed (streamBacked; output via PublishFrame→frameLog). The
	// frames are OPAQUE: the core buffers + delivers them to attachers; the
	// embedder that publishes them decides what they mean. streamMu guards
	// these; it is never held together with s.mu (PublishFrame releases streamMu
	// before taking s.mu for the lastActiveAt bump), so the two can't deadlock.
	streamMu      sync.Mutex
	streamBacked  bool
	frameLog      [][]byte // ordered opaque frame bodies (bounded, drop-oldest)
	frameLogBytes int      // sum of len(frameLog[i])
	frameDropped  int      // frames evicted from the front (absolute-cursor base)
	streamClosed  bool
	streamNotify  chan struct{}                        // close-and-replace wake for blocked readers
	inputSink     func([]byte) error                   // reverse channel: client input → producer
	controlSink   func(kind string, body []byte) error // out-of-band control → producer

	// closeHooks run once (outside the lock) when the session is Closed or
	// Killed. A generic per-session cleanup seam: an embedder registers
	// teardown of a session-scoped resource (e.g. cancel a producer goroutine,
	// reap an external process). Guarded by s.mu.
	closeHooks []func()

	// labels are generic client-facing key/value metadata an embedder attaches
	// to a session (e.g. "kind"=agent), surfaced in SessionInfo.Labels so a
	// client can categorise sessions. Set before the session enters the
	// registry; the core never interprets them. Guarded by s.mu.
	labels map[string]string

	closed bool
}

// TryStartRecover returns a context for a new recovery sequence plus
// the generation token that identifies it, cancelling any in-flight
// recovery first. The caller must pass the returned generation to
// ClearRecover when the sequence finishes.
func (s *Session) TryStartRecover() (context.Context, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoverCancel != nil {
		s.recoverCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.recoverCancel = cancel
	s.recoverGen++
	return ctx, s.recoverGen
}

// ClearRecover frees the recovery slot, but only if gen still
// identifies the active recovery. A recovery that was already
// superseded (cancelled by a newer TryStartRecover) must NOT clear
// the slot its successor now owns: otherwise a later recovery would
// see an empty slot and fail to cancel that successor, reopening the
// overlapping-recovery behaviour the slot exists to prevent. The
// generation check makes the clear identity-aware. (Function values
// aren't comparable in Go, so we key on a generation counter rather
// than the cancel func itself.)
func (s *Session) ClearRecover(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoverGen != gen {
		return
	}
	s.recoverCancel = nil
}

// NewSession constructs a Session. The caller is expected to start
// the pump goroutine separately (see Pump). We don't do it inside the
// constructor so test code can inject deterministic behaviour.
//
// `name` is the user-visible label addressable via Registry.LookupByName
// and surfaced in `mtroamd list`. The empty string is allowed —
// such a session is anonymous (registry doesn't index it by name)
// but the daemon's spawnSession synthesises a non-empty default
// (`session-<6hex>`) so client UIs never see blank names. `name` is
// immutable post-construction; rename support is a future addition.
//
// idleTimeout = 0 means "inherit the registry's default at GC
// time"; the registry's Sweep falls back to its own idleTimeout
// when this field is zero. Pass an explicit duration to give this
// session a per-session lifetime independent of the daemon-wide
// default.
// LastSidecarSeq returns the highest sidecar outSeq the daemon has
// durably committed to this session's ring. Used by the discovery
// path to compute the FrameResume(from_seq) sent to a reattached
// sidecar. Returns 0 for fresh sessions (no sidecar bytes consumed
// yet) and for sessions hydrated from pre-v0.6 meta.cbor (which
// didn't carry the lcs field).
func (s *Session) LastSidecarSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSidecarSeq
}

// AdvanceSidecarSeq updates the watermark monotonically. No-op for
// values older than the current. Called by Pump after a successful
// buf.Write; coalesced via Pump's own ack thresholds.
func (s *Session) AdvanceSidecarSeq(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.lastSidecarSeq {
		s.lastSidecarSeq = seq
	}
}

// SetPersist sets whether this session should be snapshotted to disk.
// Used by the daemon when spawning a session (after resolving the
// client-requested value against the daemon-wide default) and by
// LoadPersisted when restoring (the persisted bit is round-tripped).
func (s *Session) SetPersist(p bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persist = p
}

// Persist reports whether this session is opted into disk
// snapshotting. The flusher goroutine reads this to decide whether
// to write; the GC sweep reads it to decide whether to delete the
// on-disk dir on reap.
func (s *Session) Persist() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persist
}

// SetHookInstalled records whether the sidecar seeded a working
// prompt hook into this session's shell. The daemon calls it from the
// ptyclient spawn result at spawn time and on each lazy respawn.
// Passing nil marks the state unknown (no hook-aware spawn observed).
func (s *Session) SetHookInstalled(v *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hookInstalled = v
}

// HookInstalled reports whether a working live-inject prompt hook is
// installed in this session's shell. nil = unknown; *true = installed
// (bash/zsh); *false = no working hook. HandleAllocate reads this on
// both spawn and reattach to fill AllocateResponse.HookInstalled.
func (s *Session) HookInstalled() *bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookInstalled
}

// SetShimReady records whether this session's shell was spawned with the
// secret-broker shim dir on PATH. The daemon calls it true at spawn and
// on each lazy respawn (both run shimSpawnEnv). nil marks it unknown.
func (s *Session) SetShimReady(v *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shimReady = v
}

// ShimReady reports whether a brokered secret would reach this shell's
// tools (shim dir on PATH). nil = unknown (pre-broker spawn; regenerate
// the session); *true = ready. Read by HandleAllocate on spawn + reattach
// to fill AllocateResponse.ShimReady.
func (s *Session) ShimReady() *bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shimReady
}

// RestoredFromDisk reports whether this session was reconstructed
// from on-disk state at daemon startup. The protocol_handler reads
// this when emitting AttachAck so the client sees Restored=true on
// the first reattach after a daemon restart. Idempotent — reading
// doesn't clear the flag; ClearRestoredFlag does.
func (s *Session) RestoredFromDisk() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoredFromDisk
}

// SetRestoredFromDisk is used by LoadPersisted to mark a freshly
// hydrated session. Package-private would be nicer but we don't
// export here — the only external caller is the persistence loader
// in the same package.
func (s *Session) setRestoredFromDisk(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoredFromDisk = v
}

func NewSession(id SessionID, name string, pty PTY, rows, cols uint16, bufCapacity int, idleTimeout time.Duration) (*Session, error) {
	if pty == nil {
		return nil, errors.New("pty must not be nil")
	}
	if bufCapacity <= 0 {
		bufCapacity = DefaultBufferCapacity
	}
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
		pty:                pty,
		rows:               rows,
		cols:               cols,
		idleTimeout:        idleTimeout,
		lastActiveAt:       now,
		firstAttachPending: true,
		wedge:              newWedgeWatcher(),
		titleTracker:       &oscTitleTracker{},
		screen:             altscreen.New(int(rows), int(cols)),
	}, nil
}

// SetWedgeLogPath wires the JSONL path the wedge watcher appends to
// when it detects a candidate resize wedge (see wedgewatch.go). Empty
// path leaves slog-only logging in place. Called by the daemon after
// stateDir is resolved; safe to call at any point after NewSession.
func (s *Session) SetWedgeLogPath(path string) {
	s.mu.Lock()
	w := s.wedge
	s.mu.Unlock()
	if w != nil {
		w.SetLogPath(path)
	}
}

// WedgeAltScreenActive returns the alternate-screen state observed by
// the wedge watcher at the moment of the call. Used by the transport
// layer to populate AttachAck.AltScreenActive so iOS / mtroam clients
// can prime their local Terminal into alt-buffer mode before replay
// — without that, a truncated replay window leaves the client emulator
// stuck on the normal buffer even though the session is semantically
// on the alt screen (Claude /tui, htop, less, vim).
func (s *Session) WedgeAltScreenActive() bool {
	s.mu.Lock()
	w := s.wedge
	s.mu.Unlock()
	if w == nil {
		return false
	}
	return w.AltScreenActive()
}

// LastTitle returns the most recent terminal title observed in the
// PTY byte stream via OSC 0/2 sequences. Used by the transport layer
// to populate AttachAck.LastTitle so iOS / mtroam clients can prime
// their local Terminal with the right window title before replay —
// without that, the OSC sequence that set the title can be evicted
// from the 4 MiB ring before a long-session client reattaches, and
// the client falls back to "alt-screen but unknown TUI" (no Claude/
// Codex-specific styling on the return-to-prompt pill).
//
// Empty string when no title has been seen yet (or pre-v1.1.5
// sessions restored from disk without a persisted title).
func (s *Session) LastTitle() string {
	s.mu.Lock()
	t := s.titleTracker
	s.mu.Unlock()
	if t == nil {
		return ""
	}
	return t.Title()
}

// SetLastTitle seeds the title tracker from persisted state. Called
// by loadSessionFromDir after construction; mirrors the alt-screen
// SetAltScreenActive path.
func (s *Session) SetLastTitle(t string) {
	s.mu.Lock()
	tt := s.titleTracker
	s.mu.Unlock()
	if tt != nil {
		tt.SetTitle(t)
	}
}

// ForegroundReporter is implemented by PTY backends that can report
// the terminal's current foreground command name (the sidecar-backed
// ptyclient.Conn; v1.6.1+) and, v1.6.3+, its working directory.
// Backends without the capability simply don't conform and
// ForegroundComm/ForegroundCwd report "".
//
// The transition ANCHORS (fg_since time + fg_seq byte position) are
// NOT on this interface: the byte anchor must live in the ring's
// post-filter sequence space (what the client sees), which the backend
// doesn't know. Session derives both anchors itself by observing
// ForegroundComm in its Pump — see observeForegroundAnchor.
type ForegroundReporter interface {
	ForegroundComm() string
	ForegroundCwd() string
}

// ForegroundKiller is implemented by PTY backends that can SIGTERM the
// terminal's foreground process group without tearing down the session
// (v1.6.3+) — the daemon side of the kill-and-resume restart.
type ForegroundKiller interface {
	// KillFg SIGTERMs the foreground group. expectAgent (when non-empty) is the
	// foreground command the caller believes is running; the backend re-reads the
	// LIVE foreground and refuses unless it still matches (H1).
	KillFg(expectAgent string) error
}

// ForegroundComm returns the bare command name of the session PTY's
// current foreground process group ("claude", "codex", "vim", …) —
// kernel truth via the sidecar's tcgetpgrp poller, ≤5s fresh. "" =
// unknown: no PTY yet (restored session before lazy spawn), a
// pre-v1.6.1 sidecar, a non-Linux host, or no resolvable foreground
// process. Feeds AttachAck.Fg, AgentNotify, and SessionInfo.Fg.
// Deliberately NOT persisted — it's live state, recomputed once the
// PTY exists.
func (s *Session) ForegroundComm() string {
	s.mu.Lock()
	pty := s.pty
	s.mu.Unlock()
	if fr, ok := pty.(ForegroundReporter); ok {
		return fr.ForegroundComm()
	}
	return ""
}

// ForegroundCommSince returns the wall-clock time the foreground
// command last transitioned to its current value (v1.6.3+), or the
// zero time when unknown. Derived by observeForegroundAnchor; feeds
// AttachAck.FgSince / AgentNotify.
func (s *Session) ForegroundCommSince() time.Time {
	s.fgAnchorMu.Lock()
	defer s.fgAnchorMu.Unlock()
	return s.fgAnchorTime
}

// ForegroundSinceSeq returns the ring output byte high-water at the
// last foreground transition — the size-signal anchor, in the same
// post-filter sequence space the client tracks (buf.HeadSeq), so a
// client's currentSeq-FgSinceSeq is well defined. 0 when unknown.
// v1.6.3+. Feeds AttachAck.FgSinceSeq / AgentNotify.
func (s *Session) ForegroundSinceSeq() uint64 {
	s.fgAnchorMu.Lock()
	defer s.fgAnchorMu.Unlock()
	return s.fgAnchorSeq
}

// observeForegroundAnchor stamps the foreground transition anchors
// (wall-clock time + ring byte high-water) whenever the PTY's
// foreground command changes. Called from Pump after each ring write
// — so fgAnchorSeq captures buf.HeadSeq() at the transition, in the
// client-visible post-filter sequence space — and from the attach /
// notify paths so the anchors stay fresh on an idle session whose fg
// changed without producing output. Cheap: a cached-fg read + string
// compare; writes only on an actual change, so the first observer of
// a new value wins and later same-value observers no-op.
func (s *Session) observeForegroundAnchor() string {
	comm := s.ForegroundComm() // "" for backends without the capability
	s.fgAnchorMu.Lock()
	if !s.fgAnchorInit || comm != s.fgAnchorComm {
		s.fgAnchorInit = true
		s.fgAnchorComm = comm
		s.fgAnchorTime = time.Now()
		s.fgAnchorSeq = s.buf.HeadSeq()
	}
	s.fgAnchorMu.Unlock()
	return comm
}

// ForegroundSnapshot returns the foreground anchors as ONE consistent reading for
// AttachAck / AgentNotify. It first refreshes the anchor (critic #1: so a fg
// transition on an idle session — no output to drive Pump's observe — is reflected),
// then returns the comm it observed together with the matching since/sinceSeq under a
// single anchor-lock hold (critic #2: the prior code read Fg once but FgSince/
// FgSinceSeq/Cwd separately, so a transition in the gap shipped a torn record). cwd is
// a best-effort companion read (the `cd <cwd>` restart form is not yet wired).
func (s *Session) ForegroundSnapshot() (comm string, since time.Time, sinceSeq uint64, cwd string) {
	comm = s.observeForegroundAnchor()
	cwd = s.ForegroundCwd()
	s.fgAnchorMu.Lock()
	since, sinceSeq = s.fgAnchorTime, s.fgAnchorSeq
	s.fgAnchorMu.Unlock()
	return
}

// ForegroundCwd returns the foreground process's working directory
// (v1.6.3+), or "" when unknown. Feeds AttachAck.Cwd / AgentNotify;
// foundation for the kill-and-resume restart.
func (s *Session) ForegroundCwd() string {
	s.mu.Lock()
	pty := s.pty
	s.mu.Unlock()
	if fr, ok := pty.(ForegroundReporter); ok {
		return fr.ForegroundCwd()
	}
	return ""
}

// KillForeground SIGTERMs the session PTY's foreground process group,
// leaving the session and its shell alive (v1.6.3+). No-op (nil) on
// backends without the capability. The daemon side of kill-and-resume.
func (s *Session) KillForeground(expectAgent string) error {
	s.mu.Lock()
	pty := s.pty
	s.mu.Unlock()
	if fk, ok := pty.(ForegroundKiller); ok {
		return fk.KillFg(expectAgent)
	}
	return nil
}

// WedgeSnapshot returns the per-session cumulative metrics tracked by
// the wedge watcher. Used by `mtroamd wedge-report` and
// `mtroamd session-info` to render a summary alongside the
// JSONL contents.
func (s *Session) WedgeSnapshot() (totalOut, resizes, silent, cursor, verticalWalk uint64) {
	s.mu.Lock()
	w := s.wedge
	s.mu.Unlock()
	if w == nil {
		return 0, 0, 0, 0, 0
	}
	return w.Snapshot()
}

// OnWedge wires (or clears, with nil) a callback the wedge watcher
// invokes on every detection. The transport layer installs this when
// an exclusive client attaches so the daemon can push a
// protocol.WedgeDetected frame on the existing control stream.
// Cleared on detach so a re-attach gets a fresh subscriber without
// holding a stale closure that would write into a torn-down stream.
func (s *Session) OnWedge(cb func(WedgeNotice)) {
	s.mu.Lock()
	w := s.wedge
	s.mu.Unlock()
	if w != nil {
		w.SetOnWedge(cb)
	}
}

// SetPTYByteObserver installs (or clears, with nil) a callback that
// receives every chunk Pump reads from the PTY. Used by the recovery
// sequencer to scan for the bookend markers Claude prints during a
// save ("Commencing Save…" / "Memory Updated, restoring…"). The
// callback fires from the Pump goroutine — keep it non-blocking and
// internally thread-safe. Identity-keyed by gen (the recover generation,
// M4): the slot records the installing gen, and ClearPTYByteObserver
// only clears if it still matches — so a superseded recover's deferred
// cleanup can't nuke the live recover's observer (which would make its
// idle gate see a permanently "quiet" PTY and fire a premature kill).
func (s *Session) SetPTYByteObserver(gen uint64, cb func([]byte)) {
	s.mu.Lock()
	s.ptyByteObserver = cb
	s.ptyByteObserverGen = gen
	s.mu.Unlock()
}

// ClearPTYByteObserver clears the observer slot only if gen still owns it.
func (s *Session) ClearPTYByteObserver(gen uint64) {
	s.mu.Lock()
	if s.ptyByteObserverGen == gen {
		s.ptyByteObserver = nil
	}
	s.mu.Unlock()
}

// SuppressWedgeUntil silences all wedge detections on this session
// until the given wall-clock time. Used by the recovery sequencer to
// gate the false-positive storm from `claude --continue` scrollback
// replay (lots of CUDs in milliseconds, no real wedge). Pass a
// zero-value time.Time to clear suppression.
func (s *Session) SuppressWedgeUntil(t time.Time) {
	s.mu.Lock()
	w := s.wedge
	s.mu.Unlock()
	if w != nil {
		w.SuppressUntil(t)
	}
}

// ResetWedge clears the wedge watcher's accumulated counters and
// in-flight detection state after a deliberate Claude restart
// (`claude --continue`). The fresh renderer starts with zero drift, so
// the lifetime resize/byte accumulation that the watcher carried — and
// any in-flight resize-scan window — must reset too, or the next
// keyboard resize re-trips the detector and re-pops the banner on a
// healthy session. Complements SuppressWedgeUntil (which mutes the
// transient `--continue` replay storm); this zeroes the underlying state.
func (s *Session) ResetWedge() {
	s.mu.Lock()
	w := s.wedge
	s.mu.Unlock()
	if w != nil {
		w.ResetWedge()
	}
}

// ConsumeFirstAttach atomically reads and clears the firstAttachPending
// flag. Returns true on the first call for a given session and false on
// every subsequent call. The protocol_handler invokes this immediately
// before marshalling AttachAck so clients see FreshlyCreated=true on
// exactly the AttachAck that follows the first successful Acquire.
//
// Restored sessions arrive with the flag already cleared (LoadPersisted
// sets it to false) — a session reconstructed from disk has, by
// definition, been attached before; Restored=true conveys that.
func (s *Session) ConsumeFirstAttach() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.firstAttachPending
	s.firstAttachPending = false
	return v
}

// Name returns the user-visible session label. Empty for anonymous
// sessions (legacy callers that don't supply a name).
func (s *Session) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// setName mutates the Session's user-visible label under its lock.
// Package-private: only the Registry calls this, as part of an
// atomic rename that ALSO updates the byName index. Callers from
// outside the package must use Registry.Rename to keep the
// indices in lockstep.
func (s *Session) setName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

// LastActiveAt returns the wall-clock time of the most recent
// activity event (PTY output, stdin write, resize, or attach).
// Symmetric with IdleFor; ListSessions uses this for the picker
// UI's "last active" hint.
func (s *Session) LastActiveAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActiveAt
}

// IsAttached reports whether at least one client is currently
// attached to this session, regardless of mode. Used by
// ListSessions to surface the AttachedNow flag in the picker.
// This is a snapshot — clients can come and go between this read
// and the caller observing the result.
func (s *Session) IsAttached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients) > 0
}

// IdleTimeout returns the per-session GC timeout configured at
// construction. Returns zero when the session was constructed with
// `idleTimeout == 0` (registry-default fallback).
func (s *Session) IdleTimeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idleTimeout
}

// SetIdleTimeout updates the per-session GC timeout. Used by the
// daemon's reattach path: when an iOS client edits its host's
// Keep-alive setting, the next `mtroamd connect` carries a new
// --idle-timeout value; without this setter the existing session
// kept its original value and the GC reaped it at the OLD interval.
//
// Pass zero to revert to the registry's default. Callers should
// clamp via Registry.ResolveIdleTimeout first if they want the
// operator's --max-idle-timeout ceiling applied.
func (s *Session) SetIdleTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idleTimeout = d
}

// ID returns the session's hex-encoded identifier.
func (s *Session) ID() SessionID { return s.id }

// Created returns the wall-clock time the session was created.
func (s *Session) Created() time.Time { return s.created }

// Buffer exposes the underlying ring buffer for replay reads. Returns
// nil if the session has been closed.
func (s *Session) Buffer() *RingBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.buf
}

// InjectOutput writes p into the session's OUTPUT ring as if the PTY had
// produced it: it advances headSeq and reaches every attached client, and
// serializes with the PTY pump on the ring's own mutex (RingBuffer.Write), so
// an injected sequence can't interleave mid-escape with real PTY output. Used
// to re-emit stable alt-screen rows (the footer block) into the stream on
// attach, so a reattaching client reconstructs them from the NORMAL replay
// window — no synthetic seqs, no client change. Returns the new head seq.
func (s *Session) InjectOutput(p []byte) (uint64, error) {
	buf := s.Buffer()
	if buf == nil {
		return 0, ErrSessionClosed
	}
	if _, err := buf.Write(p); err != nil {
		return 0, err
	}
	return buf.HeadSeq(), nil
}

// WriteStdin forwards bytes from the client to the PTY's input.
// Updates the activity timestamp so GC doesn't reap an active session
// just because output has gone quiet.
func (s *Session) WriteStdin(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrSessionClosed
	}
	pty := s.pty
	s.lastActiveAt = time.Now()
	s.mu.Unlock()
	return pty.Write(p)
}

// IsCurrentExclusive reports whether gen identifies the session's CURRENT
// exclusive client. The transport checks this LIVE — right before writing client
// stdin, applying a Resize, or starting a Recover — instead of trusting the
// goroutine-local mode captured at attach. A displaced exclusive client's readPump
// goroutine still holds mode==AttachExclusive until its CancelRead lands, and a
// frame it already pulled off the wire would otherwise be applied to the session
// the NEW owner now controls. Once displaced, the client's gen is no longer in
// s.clients, so this returns false and the in-flight frame is dropped.
// (M3, security audit v1.7.0.)
func (s *Session) IsCurrentExclusive(gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		if c.gen == gen {
			return c.mode == AttachExclusive
		}
	}
	return false
}

// Resize updates the PTY's window size and remembers the latest
// values for any future re-attachers that join without sending their
// own Resize. If the new size differs from the current one, the
// kernel fires SIGWINCH at the child shell — bash redraws its prompt,
// alt-screen TUIs (Claude Code fullscreen, htop, less, vim) repaint
// their frame.
//
// Earlier versions armed a one-shot drop-the-next-chunk filter here
// against `\r\x1b[K` to keep bash's SIGWINCH prompt redraw out of the
// ring buffer (kept replays clean). That filter is removed: the 4-byte
// match is too broad and silently swallows the first redraw chunk of
// any alt-screen renderer whose redraw starts the same way, breaking
// the visible state for the user. Cost of the removal is one extra
// prompt-redraw blob per bash resize in the ring buffer (~80 bytes,
// renders as one duplicate prompt line on replay) — well worth it to
// keep Claude/htop/vim rendering correctly.
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		slog.Warn("session.Resize: session closed — dropping",
			"sid", s.id.String(), "rows", rows, "cols", cols)
		return ErrSessionClosed
	}
	oldRows, oldCols := s.rows, s.cols
	s.rows, s.cols = rows, cols
	pty := s.pty
	s.lastActiveAt = time.Now()
	s.mu.Unlock()
	if pty == nil {
		// Stream-backed session (or one whose PTY isn't spawned yet): geometry
		// is recorded above, but there's no terminal to size. No-op, not a panic.
		return nil
	}
	if oldRows == rows && oldCols == cols {
		slog.Debug("session.Resize: dimensions unchanged — calling SetSize anyway",
			"sid", s.id.String(), "rows", rows, "cols", cols)
	} else {
		slog.Info("session.Resize: dimensions changed",
			"sid", s.id.String(),
			"from", fmt.Sprintf("%dx%d", oldCols, oldRows),
			"to", fmt.Sprintf("%dx%d", cols, rows))
	}
	err := pty.SetSize(rows, cols)
	if err != nil {
		slog.Warn("session.Resize: PTY SetSize failed",
			"sid", s.id.String(), "err", err)
	} else {
		slog.Debug("session.Resize: PTY SetSize OK — SIGWINCH should fire",
			"sid", s.id.String(), "rows", rows, "cols", cols)
	}
	// Arm the wedge watcher only on a successful SetSize and only
	// when the geometry actually changed. A no-op resize wouldn't
	// trigger SIGWINCH, so there's no redraw to wait for.
	if err == nil && (oldRows != rows || oldCols != cols) {
		if s.wedge != nil {
			s.wedge.ArmResize(oldRows, rows, cols, s.created)
		}
	}
	// Keep the live screen-model geometry in lockstep with the PTY, but
	// only when the geometry actually changed (matching the SIGWINCH the
	// app acts on). We resize regardless of the SetSize error: the model
	// tracks the client's requested geometry, and the app will redraw to
	// it once SIGWINCH lands.
	if oldRows != rows || oldCols != cols {
		if s.screen != nil {
			s.screenMu.Lock()
			s.screen.Resize(int(rows), int(cols))
			s.screenMu.Unlock()
		}
	}
	return err
}

// InjectAltScreenRepaint atomically snapshots the live screen-model and,
// when a full-screen TUI is running (alt buffer) AND the model is still
// faithful, injects a synthesized clean full-frame redraw into the output
// ring — returning the PRE-inject head seq (start) and true. The caller
// pins the replay window to `start` so the client replays ONLY the redraw
// (+ any live tail after it), instead of the truncated raw byte tail it
// cannot reassemble into a 2-D screen (the cold-start alt-screen spill).
//
// Snapshot, head capture, and injection all run under screenMu, which the
// Pump loop also holds around its ring-write+Feed, so no PTY chunk can land
// between the snapshot and the injection: the redraw exactly matches the
// ring prefix [.., start). Returns (0, false) when there is no model, the
// app is on the main buffer (a normal shell — raw replay is correct there),
// the model is unfaithful (unemulated op, or a sidecar gap marked it stale),
// the model is resize-dirty (geometry changed, app hasn't repainted yet — the
// grid is top-anchored/misplaced), or the session is closed; the caller then
// falls back to today's footer + raw replay, never worse than before.
// ScreenState reports the live screen-model's diagnostic state: whether a
// model exists, is on the alt buffer, and is faithful. For attach-path
// logging only; racy by nature (Pump may feed between calls).
func (s *Session) ScreenState() (has, alt, faithful bool) {
	if s.screen == nil {
		return false, false, false
	}
	s.screenMu.Lock()
	defer s.screenMu.Unlock()
	return true, s.screen.AltActive(), s.screen.Faithful()
}

// ScreenResizeDirty reports whether the live screen-model was geometry-changed
// but the app has not yet repainted to the new size (see altscreen.ResizeDirty).
// The attach path gates both the full-frame redraw and the footer re-emit on
// this: a dirty model is top-anchored to the new geometry and would ship a
// misplaced frame. Returns false when there is no model. Racy for logging;
// InjectAltScreenRepaint re-checks it authoritatively under the same lock.
func (s *Session) ScreenResizeDirty() bool {
	if s.screen == nil {
		return false
	}
	s.screenMu.Lock()
	defer s.screenMu.Unlock()
	return s.screen.ResizeDirty()
}

func (s *Session) InjectAltScreenRepaint() (start uint64, ok bool) {
	if s.screen == nil {
		return 0, false
	}
	s.screenMu.Lock()
	defer s.screenMu.Unlock()
	if !s.screen.AltActive() || !s.screen.Faithful() || s.screen.ResizeDirty() {
		return 0, false
	}
	buf := s.Buffer()
	if buf == nil {
		return 0, false
	}
	redraw := s.screen.Repaint()
	if len(redraw) == 0 {
		return 0, false
	}
	start = buf.HeadSeq()
	if _, err := buf.Write(redraw); err != nil {
		return 0, false
	}
	return start, true
}

// WindowSize returns the latest known window size.
func (s *Session) WindowSize() (rows, cols uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows, s.cols
}

// Acquire claims this session for a new attach with the given mode.
//
// Semantics:
//
//   - mode = AttachExclusive: any prior exclusive client is
//     displaced (its context cancelled, goroutine should observe
//     ctx.Done() and exit with reason "replaced"). Existing
//     readonly clients are unaffected — they keep observing.
//   - mode = AttachReadonly: never displaces anyone. Coexists with
//     a current exclusive client and with other readonly clients.
//   - mode = AttachExclusiveIfFree: resolved here, atomically — to
//     AttachExclusive when no live exclusive client exists, else to
//     AttachReadonly. Never displaces anyone.
//
// Returns a derived context the new attacher should use; cancelling
// that context (e.g., via Release or via the registry GC'ing the
// session) terminates the new attach. `gen` is the unique identity
// of THIS attach — the caller must pass it to Release later. The
// returned AttachMode is the mode actually granted; it differs from
// the requested mode only for AttachExclusiveIfFree.
func (s *Session) Acquire(parent context.Context, mode AttachMode) (context.Context, uint64, AttachMode, error) {
	return s.AcquireClient(parent, mode, "")
}

// AcquireClient is Acquire with a stable per-device client identity. An
// exclusive-if-free attach whose clientID is non-empty and equals the current
// exclusive holder's resolves to exclusive — silently displacing that same
// client's stale connection — rather than readonly. clientID "" reproduces
// Acquire's behaviour exactly (so the bare Acquire and all existing callers are
// unchanged). The only production caller is the transport attach path.
func (s *Session) AcquireClient(parent context.Context, mode AttachMode, clientID string) (context.Context, uint64, AttachMode, error) {
	ctx, gen, granted, doomed, err := s.acquireLocked(parent, mode, clientID)
	// Notify + cancel displaced clients OUTSIDE the lock: a
	// notifyReplaced hook does a network write (Goodbye{replaced})
	// that can block on a dead peer's flow-control window — under
	// s.mu that would wedge every session operation until the
	// transport's write backstop fired.
	//
	// The notify is BOUNDED, not synchronous: the write is mutex-
	// serialised against the displaced connection's output pump, and
	// when that peer died without a FIN (app killed by an update, or
	// the client's in-process tsnet node torn down in a recycle) the
	// pump sits blocked mid-write into a full send buffer, holding the
	// mutex indefinitely — an unbounded notify then stalls THIS Acquire,
	// and with it the NEW attach's Ack, until the client's attach-ack
	// timeout fires (device-observed: every post-recycle/post-install
	// reattach paid a full timeout + retry cycle). A healthy displaced
	// peer's Goodbye push completes in single-digit milliseconds. A
	// live-but-backpressured peer (>1s of flow-control stall on a
	// congested link) can miss its Goodbye and see a bare drop instead —
	// deliberate: when it redials, the exclusive-if-free clientID rule
	// lands it readonly with the Take Over affordance (different client)
	// or silently reclaims its own slot (same client), so the loss is a
	// softer banner, never an attach-war.
	//
	// Invariants preserved: notify-before-cancel (a completing notifier
	// still finishes before its context is torn down, so the frame goes
	// out on a healthy stream), and the displaced context is always
	// cancelled by the time Acquire returns. The cancel tears the
	// transport down, which unblocks the stuck pump write; an abandoned
	// notifier's late writeFrame then fails fast and is discarded.
	for _, c := range doomed {
		if c.notifyReplaced != nil {
			notified := make(chan struct{})
			go func(notify func(), gen uint64) {
				defer close(notified)
				// A panicking notifier must not break the
				// displacement chain for the remaining doomed
				// clients (their cancel would never fire).
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("session: replaced-notifier panic",
							"sid", s.ID().String(), "gen", gen, "panic", r)
					}
				}()
				notify()
			}(c.notifyReplaced, c.gen)
			select {
			case <-notified:
			case <-time.After(replacedNotifyTimeout):
				slog.Warn("session: replaced-notifier timed out — cancelling displaced client anyway",
					"sid", s.ID().String(), "gen", c.gen)
			}
		}
		c.cancel()
	}
	return ctx, gen, granted, err
}

// replacedNotifyTimeout bounds how long a displacement waits for the
// Goodbye{replaced} push to a doomed client before cancelling it anyway.
// Generous against a healthy peer (the push is a buffered write, done in
// milliseconds) while keeping a dead peer's wedged output pump from
// stalling the displacing attach's Ack (see AcquireClient).
const replacedNotifyTimeout = 1 * time.Second

// acquireLocked is Acquire's lock-holding core. It returns the
// displaced clients (doomed) instead of cancelling them so the
// caller can run the notify+cancel sequence after s.mu is released.
func (s *Session) acquireLocked(parent context.Context, mode AttachMode, clientID string) (context.Context, uint64, AttachMode, []sessionClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, 0, mode, nil, ErrSessionClosed
	}
	if mode == AttachExclusiveIfFree {
		mode = AttachExclusive
		for _, c := range s.clients {
			if c.mode == AttachExclusive {
				// Same client (non-empty matching id) → this is that client's own
				// reconnect/cold-start; keep exclusive so the displacement below
				// silently evicts its stale connection. Different/unknown client →
				// downgrade to readonly ("Live on another device").
				// Constant-time compare for parity with the SID check (ClientID is a
				// non-secret hint, so this is belt-and-suspenders). Differing lengths
				// → ConstantTimeCompare returns 0 (non-match), as intended.
				if clientID != "" && subtle.ConstantTimeCompare([]byte(c.clientID), []byte(clientID)) == 1 {
					break
				}
				mode = AttachReadonly
				break
			}
		}
	}
	if mode == AttachPassive {
		// Passive attaches live in a sibling slice so they're
		// invisible to AttachedModes / PeerModes by construction.
		// Cap enforced here so the transport layer doesn't need to
		// peek into Session internals.
		if len(s.passiveClients) >= MaxPassivePerSession {
			return nil, 0, mode, nil, ErrPassiveCapacity
		}
		s.nextGen++
		gen := s.nextGen
		ctx, cancel := context.WithCancel(parent)
		s.passiveClients = append(s.passiveClients, sessionClient{
			gen:    gen,
			mode:   mode,
			cancel: cancel,
		})
		s.lastActiveAt = time.Now()
		return ctx, gen, mode, nil, nil
	}
	var doomed []sessionClient
	if mode == AttachExclusive {
		// Displace any current exclusive client. We collect the
		// doomed entries and drop them from the slice; Acquire
		// notifies + cancels them after the lock is released.
		// Passive attachers are untouched — exclusive turnover is
		// invisible to them.
		kept := s.clients[:0]
		for _, c := range s.clients {
			if c.mode == AttachExclusive {
				doomed = append(doomed, c)
				continue
			}
			kept = append(kept, c)
		}
		s.clients = kept
	}
	if mode == AttachReadonly {
		// Cap read-only fan-out (see MaxReadonlyPerSession). Counts
		// only read-only entries; the single exclusive client lives in
		// the same slice but is bounded by displacement above.
		n := 0
		for _, c := range s.clients {
			if c.mode == AttachReadonly {
				n++
			}
		}
		if n >= MaxReadonlyPerSession {
			return nil, 0, mode, nil, ErrReadonlyCapacity
		}
	}
	s.nextGen++
	gen := s.nextGen
	ctx, cancel := context.WithCancel(parent)
	s.clients = append(s.clients, sessionClient{
		gen:      gen,
		mode:     mode,
		cancel:   cancel,
		clientID: clientID,
	})
	s.lastActiveAt = time.Now()
	return ctx, gen, mode, doomed, nil
}

// SetReplacedNotifier installs the displacement callback on the
// client identified by gen (see sessionClient.notifyReplaced). The
// transport layer calls this right after a successful exclusive
// Acquire, once its write path exists. No-op for a stale gen (the
// client was already displaced or released) and for passive clients
// (they are never displaced).
func (s *Session) SetReplacedNotifier(gen uint64, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.clients {
		if s.clients[i].gen == gen {
			s.clients[i].notifyReplaced = fn
			return
		}
	}
}

// Release is called by an attached client when its goroutine exits.
// Removes the client identified by `gen` from the active-clients
// slice. Idempotent — a stale gen (we were already displaced and
// removed) is a no-op, so a displaced caller calling Release after
// the new owner has taken over does NOT stomp the new owner's
// state.
func (s *Session) Release(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.clients {
		if c.gen == gen {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			return
		}
	}
	// Passive clients live in a parallel slice; check it too.
	for i, c := range s.passiveClients {
		if c.gen == gen {
			s.passiveClients = append(s.passiveClients[:i], s.passiveClients[i+1:]...)
			return
		}
	}
}

// HasExclusiveStdinWriter reports whether at least one currently-
// attached client is in AttachExclusive mode. Used by readonly-
// pump validation paths that want to log "exclusive client should
// have written this stdin, not the readonly attempting it" — but
// the pumps don't currently need to assert that, so this method is
// reserved for future telemetry.
func (s *Session) HasExclusiveStdinWriter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		if c.mode == AttachExclusive {
			return true
		}
	}
	return false
}

// AttachedModes returns the modes of every currently-attached
// client. Snapshot — clients can come and go between this read
// and the caller using the result. Used by ListSessions /
// session-info to render multi-attach state in pickers and CLI
// tools.
func (s *Session) AttachedModes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, c.mode.String())
	}
	return out
}

// PeerModes returns a snapshot of attached clients' modes excluding
// the caller's gen. Used to populate `AttachAck.Peers` so a
// freshly-attaching client can render a "also attached: 1
// readonly" hint without needing a separate IPC roundtrip.
func (s *Session) PeerModes(excludingGen uint64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.clients))
	for _, c := range s.clients {
		if c.gen == excludingGen {
			continue
		}
		out = append(out, c.mode.String())
	}
	return out
}

// Touch refreshes the activity timestamp without changing any other
// state. Called by the pump goroutine on PTY output.
func (s *Session) Touch() {
	s.mu.Lock()
	if !s.closed {
		s.lastActiveAt = time.Now()
	}
	s.mu.Unlock()
}

// IdleFor returns how long ago the session last saw activity (PTY
// output, stdin write, resize, or attach). Used by the registry's GC
// sweep.
func (s *Session) IdleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastActiveAt)
}

// Closed reports whether Close has been called.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close terminates the PTY (which sends SIGHUP to the child),
// cancels every attached client, stops the persistence flusher
// (if running), and marks the session unusable. Safe to call
// multiple times; subsequent calls return nil.
//
// Close intentionally does NOT delete the session's on-disk
// persistence directory — that decision belongs to the caller:
// Registry.Remove / Sweep call DeletePersisted afterward (Kill
// or idle-GC reap should drop the on-disk state), but
// Registry.Shutdown (daemon-wide shutdown) leaves it so the next
// daemon start can restore.
//
// Close is the graceful path: when the PTY is a sidecar.Conn, this
// just closes the socket — the sidecar enters its grace timer and
// will be reattached on next daemon startup. Use Kill instead when
// the caller wants the child shell reaped immediately (e.g. user-
// invoked `mtroam kill`).
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pty := s.pty
	// Snapshot cancel funcs and clear the slice — we'll fire them
	// outside the lock so a goroutine's Release-on-exit doesn't
	// deadlock on us.
	cancels := make([]context.CancelFunc, 0, len(s.clients)+len(s.passiveClients))
	for _, c := range s.clients {
		cancels = append(cancels, c.cancel)
	}
	for _, c := range s.passiveClients {
		cancels = append(cancels, c.cancel)
	}
	hooks := s.closeHooks
	s.closeHooks = nil
	s.clients = nil
	s.passiveClients = nil
	s.mu.Unlock()

	// Synchronously stop the flusher BEFORE closing the PTY. This
	// gives the flusher's ctx-done path a chance to do its final
	// SaveTo before we mark the session closed (and on daemon
	// shutdown that final write is what preserves the session for
	// the next daemon start).
	s.stopFlusher()

	for _, c := range cancels {
		c()
	}
	for _, h := range hooks {
		h()
	}
	if pty != nil {
		return pty.Close()
	}
	return nil
}

// RegisterCloseHook adds fn to the set run once (outside the lock) when the
// session is Closed or Killed. Generic per-session cleanup: an embedder uses it
// to tear down a session-scoped resource it created. No-op after the session has
// already closed (the hook would never fire), so callers should register before
// the session can be reaped.
func (s *Session) RegisterCloseHook(fn func()) {
	s.mu.Lock()
	s.closeHooks = append(s.closeHooks, fn)
	s.mu.Unlock()
}

// SetLabel attaches a generic client-facing label to the session (surfaced in
// SessionInfo.Labels). Intended to be called once, before the session enters
// the registry; the core never interprets the key or value.
func (s *Session) SetLabel(key, val string) {
	s.mu.Lock()
	if s.labels == nil {
		s.labels = make(map[string]string)
	}
	s.labels[key] = val
	s.mu.Unlock()
}

// Labels returns a copy of the session's client-facing labels, or nil if none.
func (s *Session) Labels() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.labels))
	for k, v := range s.labels {
		out[k] = v
	}
	return out
}

// PTYKiller is the optional capability a session.PTY can implement
// to request immediate teardown of the underlying process tree (vs.
// the graceful socket-close that PTY.Close performs). The sidecar-
// backed ptyclient.Conn implements this by writing a die_now frame
// before closing the socket; the in-process pty.Handle does not
// implement this (its Close already reaps the child).
type PTYKiller interface {
	Kill() error
}

// Kill is the immediate-teardown sibling of Close. For sidecar-
// backed PTYs (the v0.6+ mtRoam sessions) it sends die_now so the
// child shell is SIGHUP'd within ~250 ms. For in-process PTYs it
// falls back to Close (which already SIGHUPs synchronously).
//
// Used by registry.Remove so `mtroam kill` doesn't leave the child
// shell running during the sidecar's 30s reconnect-grace window.
func (s *Session) Kill() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pty := s.pty
	cancels := make([]context.CancelFunc, 0, len(s.clients)+len(s.passiveClients))
	for _, c := range s.clients {
		cancels = append(cancels, c.cancel)
	}
	for _, c := range s.passiveClients {
		cancels = append(cancels, c.cancel)
	}
	hooks := s.closeHooks
	s.closeHooks = nil
	s.clients = nil
	s.passiveClients = nil
	s.mu.Unlock()

	s.stopFlusher()

	for _, c := range cancels {
		c()
	}
	for _, h := range hooks {
		h()
	}
	if pty == nil {
		return nil
	}
	if k, ok := pty.(PTYKiller); ok {
		return k.Kill()
	}
	return pty.Close()
}

// ErrSessionHasPTY is returned by AssignPTY when called on a session
// that already owns a PTY. Indicates a race between two attach
// handlers both trying to be the first-attach lazy spawner for a
// restored session; the loser closes its handle and continues.
var ErrSessionHasPTY = errors.New("session already has a PTY")

// AssignPTY hands ownership of a freshly-spawned PTY to a previously-
// restored Session. The caller (protocol_handler on first attach to
// a session that was hydrated by LoadPersisted) builds the *pty.Handle
// via pty.Spawn and passes it here; AssignPTY plumbs it onto s.pty
// and clears the restoredFromDisk flag so subsequent attaches see
// Restored=false on the wire.
//
// On error the caller is responsible for closing the supplied PTY —
// AssignPTY does NOT take ownership if it fails. The expected error
// is ErrSessionHasPTY (race lost to another concurrent attach);
// callers handle that by simply closing their PTY handle and
// proceeding with the normal attach path.
//
// The caller must also start the session's Pump goroutine
// (`go sess.Pump()`) after a successful AssignPTY — this method
// only wires the handle; it doesn't kick off reads.
func (s *Session) AssignPTY(p PTY) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionClosed
	}
	if s.pty != nil {
		return ErrSessionHasPTY
	}
	s.pty = p
	s.restoredFromDisk = false
	return nil
}

// StartFlusher launches the background snapshot loop. Idempotent —
// second call is a no-op (or a no-op when persist is false). The
// flusher writes via SaveTo on every interval where the buffer's
// HeadSeq has advanced past the last snapshot, plus one final write
// on stopFlusher. Failed writes are logged but don't kill the loop.
//
// `parentDir` is the daemon's state dir (the parent of `sessions/`).
// `interval` zero falls back to 30 seconds. `logger` may be nil
// (defaults to slog.Default()).
func (s *Session) StartFlusher(parentDir string, interval time.Duration, logger *slog.Logger) {
	s.mu.Lock()
	if s.flusherCancel != nil || !s.persist || s.closed {
		s.mu.Unlock()
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.flusherCancel = cancel
	s.flusherDone = make(chan struct{})
	done := s.flusherDone
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.runFlusher(ctx, parentDir, interval, logger)
	}()
}

// runFlusher is the actual snapshot loop body. Exits on ctx
// cancellation; performs a final flush before returning so daemon
// shutdown preserves the most-recent state.
//
// Two paths through the dirty check:
//
//   - Normal tick: checks `s.closed`. If the session has been killed
//     (Remove/Sweep called Close), the caller will DeletePersisted
//     shortly — skip the write to avoid recreating a dir that's
//     about to be removed.
//   - ctx-done (Close-initiated shutdown): writes UNCONDITIONALLY
//     if there's dirty state, because Close intentionally preserves
//     on-disk content. The caller (daemon shutdown) needs the
//     latest snapshot for the next start. The DeletePersisted-races-
//     with-final-flush concern doesn't apply here: Close only
//     touches on-disk after stopFlusher returns (which is after this
//     function exits), and Remove's DeletePersisted runs even later.
func (s *Session) runFlusher(ctx context.Context, parentDir string, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()

	flushIfDirty := func(force bool) {
		currentHead := s.buf.HeadSeq()
		s.mu.Lock()
		last := s.lastSnapshotSeq
		closed := s.closed
		s.mu.Unlock()
		if closed && !force {
			return
		}
		if currentHead == last {
			return
		}
		if err := s.SaveTo(parentDir); err != nil {
			logger.Warn("session.flusher.write_failed",
				"session", s.ID().String(),
				"err", err.Error(),
			)
		}
	}

	for {
		select {
		case <-ctx.Done():
			flushIfDirty(true)
			return
		case <-t.C:
			flushIfDirty(false)
		}
	}
}

// stopFlusher signals the flusher to exit and waits for it. Idempotent.
// Called from Close; package-private because the lifecycle is owned
// by Session itself, not by external callers.
func (s *Session) stopFlusher() {
	s.mu.Lock()
	cancel := s.flusherCancel
	done := s.flusherDone
	s.flusherCancel = nil
	s.flusherDone = nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Pump runs the read-PTY-into-ring-buffer loop. It blocks until the
// PTY returns io.EOF (child exited cleanly), an error, or the session
// is closed. On exit it calls Close so the registry can reap the
// session.
//
// Each PTY chunk is run through a QueryFilter that intercepts
// terminal-query escape sequences (Device Attributes, Device Status
// Report) and synthesises responses server-side — apps querying the
// terminal get answered without the query bytes ever reaching the
// ring buffer. This keeps replay-on-reattach pollution-free without
// breaking interactive apps' capability negotiation.
//
// Callers should run Pump in its own goroutine immediately after
// constructing a Session.
func (s *Session) Pump() {
	defer s.Close()
	// We allocate a small buffer; PTYs typically deliver in
	// hundreds-of-bytes chunks, occasionally a few KB. 8 KiB is more
	// than enough to not be the bottleneck.
	chunk := make([]byte, 8*1024)
	filter := NewQueryFilter(s.pty)
	seqAware, _ := s.pty.(SeqAwarePTY)
	for {
		// Surface any Trunc-before-frame from the sidecar BEFORE the
		// next Read so the daemon-ring's headSeq advances past the
		// lost span before payload bytes land on top of it.
		if seqAware != nil {
			if gap := seqAware.ConsumeTrunc(); gap > 0 {
				s.buf.AdvanceWithGap(gap)
				// The sidecar dropped bytes under backpressure, so the
				// live screen-model missed updates and can no longer be
				// trusted for a redraw. Mark it stale (it recovers on the
				// next full clear / alt-enter); the attach path falls back
				// to raw replay until then.
				if s.screen != nil {
					s.screenMu.Lock()
					s.screen.MarkStale()
					s.screenMu.Unlock()
				}
			}
		}
		n, err := s.pty.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			filtered := filter.Process(data)
			// Read this chunk's sidecar watermark up front so it advances in
			// lockstep with the ring + screen below. LastConsumedSeq is stable
			// after the Read — nothing below consumes more sidecar bytes.
			var seq uint64
			if seqAware != nil {
				seq = seqAware.LastConsumedSeq()
			}
			if len(filtered) > 0 {
				// Write to the ring, feed the live screen-model, AND advance the
				// sidecar watermark under screenMu TOGETHER, so the model's grid,
				// the ring head, and lastSidecarSeq stay in lockstep. The attach /
				// snapshot paths capture all three under the same lock
				// (InjectAltScreenRepaint / SaveTo), so no PTY chunk can land
				// between them — the redraw exactly matches the ring prefix AND the
				// sidecar watermark it pins the resume window to (a skew here would
				// make the restore resume double-feed a chunk already in the
				// persisted Repaint). Cheap: a ~rows×cols cell walk per chunk.
				// screenMu → s.mu (inside AdvanceSidecarSeq) matches the established
				// order (InjectAltScreenRepaint); no path takes s.mu → screenMu.
				s.screenMu.Lock()
				_, _ = s.buf.Write(filtered)
				if s.screen != nil {
					s.screen.Feed(filtered)
				}
				if seqAware != nil {
					s.AdvanceSidecarSeq(seq)
				}
				s.screenMu.Unlock()
				// Feed the wedge watcher with the post-filter byte
				// stream — the same bytes the client renders, so a
				// CUP row that violates the geometry will be visible
				// here too. QueryFilter only synthesises responses to
				// terminal queries (DA/DSR); it doesn't strip CUPs.
				if s.wedge != nil {
					s.wedge.ObserveBytes(filtered, s.created)
				}
				if s.titleTracker != nil {
					// OSC title sniff — captures `\x1b]2;<title>\x07`
					// (and OSC 0 / OSC 1) so the daemon knows the
					// current terminal title even after the original
					// OSC has been evicted from the 4 MiB ring. Cheap
					// state-machine walk; doesn't allocate per byte.
					s.titleTracker.feed(filtered)
				}
				// Fire the PTY byte observer (recovery sequencer's
				// marker detection). Snapshot under the lock so a
				// concurrent SetPTYByteObserver(nil) doesn't race
				// with the dereference.
				s.mu.Lock()
				obs := s.ptyByteObserver
				s.mu.Unlock()
				if obs != nil {
					obs(filtered)
				}
			}
			if seqAware != nil {
				// lastSidecarSeq was advanced in lockstep with the ring+screen
				// above when filtered was non-empty; when everything was filtered
				// out (a pure query chunk — no ring/screen change) advance it here,
				// where there is nothing it can skew. Then Ack (best-effort — a
				// network error just means the sidecar re-sends our lcs on the next
				// FrameResume) and stamp the fg anchors now that buf.HeadSeq
				// reflects this chunk (client-visible post-filter seq space).
				if len(filtered) == 0 {
					s.AdvanceSidecarSeq(seq)
				}
				_ = seqAware.Ack(seq)
				s.observeForegroundAnchor()
			}
			s.Touch()
		}
		if err != nil {
			// io.EOF or any read error means the child is gone.
			return
		}
	}
}

// ErrSessionClosed is returned by methods invoked after Close.
var ErrSessionClosed = errors.New("session is closed")

// ErrPassiveCapacity is returned by Acquire(AttachPassive) when the
// session already has MaxPassivePerSession passive watchers. Transport
// layer maps this to AttachAck.Err = AttachErrCapacity on the wire.
var ErrPassiveCapacity = errors.New("session passive-attach capacity reached")

// ErrReadonlyCapacity is returned by Acquire when a session already
// has MaxReadonlyPerSession read-only attachers.
var ErrReadonlyCapacity = errors.New("session read-only-attach capacity reached")
