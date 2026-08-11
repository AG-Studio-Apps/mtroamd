// Package ipc is the unix-socket IPC between `mtroamd serve`
// (the long-running daemon that owns the session registry) and
// `mtroamd connect` (the SSH-side helper that runs once per
// bootstrap to allocate or reattach a session).
//
// The same uid+gid runs both processes — there's no auth on the
// socket beyond filesystem permissions (the socket lives at mode
// 0600 in the daemon's state dir). Threat model: anyone who can
// read the socket can already read the daemon's state directly,
// so this layer trusts its peer.
//
// Wire format reuses the protocol package's CBOR framing (length-
// prefixed CBOR body) so we have a single understanding of "what's
// on a stream" throughout the codebase.
package ipc

// Request is the union of all `mtroamd connect` → `mtroamd serve`
// messages. Discriminated by the `t` tag, like the protocol's
// control stream.
const (
	TypeAllocate      = "Allocate"
	TypePing          = "Ping"
	TypeListSessions  = "ListSessions"
	TypeKillSession   = "KillSession"
	TypeRenameSession = "RenameSession"
	TypeStatus        = "Status"
	TypeSessionSearch = "SessionSearch"
	// Secret broker (v1.7.6+). SetSessionSecrets is the client → daemon
	// full push of a session's secret set; GetSecrets is the local
	// consumer (`mtroamd secret-exec`) asking for the values a command
	// should be exec'd with. Both are additive: older daemons reject the
	// unknown type via the dispatch default, older clients never send it.
	TypeSetSessionSecrets = "SetSessionSecrets"
	TypeGetSecrets        = "GetSecrets"
)

// SecretEntry is the CBOR wire form of one broker secret: env-var name,
// value, and the command basenames it is injected into. Distinct from
// internal/secret.Entry (which carries JSON tags for the client-staged
// file); `set-secrets` converts the parsed file into these before the
// IPC. Short cbor keys keep the frame small.
type SecretEntry struct {
	Key   string   `cbor:"k"`
	Value string   `cbor:"v"`
	Cmds  []string `cbor:"c"`
}

// SetSessionSecretsRequest REPLACES a session's broker secret set (a
// full push, not a delta). An empty Secrets clears the session. The
// daemon holds the values in memory only and (re)generates the
// session's PATH shims from the union of declared commands.
type SetSessionSecretsRequest struct {
	T         string        `cbor:"t"`
	SessionID string        `cbor:"sid"`
	Secrets   []SecretEntry `cbor:"secrets,omitempty"`
}

// SetSessionSecretsResponse is a plain ack. Ok=false + Msg on a bad
// session id or a shim-write failure.
type SetSessionSecretsResponse struct {
	T   string `cbor:"t"`
	Ok  bool   `cbor:"ok"`
	Err string `cbor:"err,omitempty"`
	Msg string `cbor:"msg,omitempty"`

	// ShimReady / ShimNotReadyReason report, at the moment of the push, whether
	// the brokered secrets will actually reach their tools. Same semantics as the
	// AllocateResponse fields.
	//
	// ★★ This is the authoritative place to ask. Allocate cannot answer it: the
	// shell announces readiness from its first PROMPT (measured ~389ms) while
	// allocate returns in ~28ms and `connect` allocates exactly once, so an
	// allocate-only answer reports not-ready for every freshly spawned session no
	// matter how healthy it is. Here the session has been running for as long as
	// it takes the user to push a secret, and - more importantly - this is the
	// call whose outcome actually depends on the answer.
	ShimReady          *bool  `cbor:"shim_ready,omitempty"`
	ShimNotReadyReason string `cbor:"shim_not_ready_reason,omitempty"`
}

// GetSecretsRequest is `secret-exec`'s ask: give me the env for
// `Command` in session `SessionID`. The caller identifies its session
// from MESHTERM_SESSION_ID (a same-uid local process; same trust model
// as the rest of this socket).
type GetSecretsRequest struct {
	T         string `cbor:"t"`
	SessionID string `cbor:"sid"`
	Command   string `cbor:"cmd"`
}

// GetSecretsResponse carries ONLY the values for the named command
// (least privilege — never the full set). Env empty (Ok=true) means the
// session has no secrets for that command; the caller then execs the
// real binary unchanged.
type GetSecretsResponse struct {
	T   string            `cbor:"t"`
	Ok  bool              `cbor:"ok"`
	Env map[string]string `cbor:"env,omitempty"`
	Err string            `cbor:"err,omitempty"`
	Msg string            `cbor:"msg,omitempty"`
}

// AllocateRequest reserves an attach for the named session (or
// creates a new one if SessionID is empty / "new") and returns the
// info needed to build the bootstrap line.
type AllocateRequest struct {
	T string `cbor:"t"`

	// SessionID, when set to the literal string "new" or empty,
	// requests a new session. Otherwise it's a 32-char hex ID
	// matching an existing session.
	SessionID string `cbor:"sid,omitempty"`

	// Rows and Cols set the initial PTY size for new sessions.
	// Ignored when reattaching to an existing session.
	Rows uint16 `cbor:"rows,omitempty"`
	Cols uint16 `cbor:"cols,omitempty"`

	// Exec is the command line to run inside the PTY for new
	// sessions. Empty means the user's $SHELL. The first element is
	// the binary path; remaining elements are args. Ignored when
	// reattaching.
	Exec []string `cbor:"exec,omitempty"`

	// Shell overrides the default shell-resolution chain. Empty
	// means use the daemon's resolveShell logic ($SHELL → /bin/bash
	// → /bin/sh). Ignored when Exec is set or when reattaching.
	Shell string `cbor:"shell,omitempty"`

	// IdleTimeoutNanos is the per-session GC timeout the client
	// is requesting. Zero means "use the daemon's default" — the
	// registry then applies whatever the operator configured via
	// `--idle-timeout` on serve. A non-zero value is clamped at
	// the daemon's `--max-idle-timeout` ceiling when one is set.
	// Ignored when reattaching: the timeout is fixed at session
	// creation; reattach inherits whatever the original Allocate
	// chose. Encoded as nanoseconds rather than time.Duration so
	// the wire form stays portable across CBOR libraries that
	// don't know about Go's Duration type.
	IdleTimeoutNanos int64 `cbor:"itn,omitempty"`

	// Name is the optional user-visible session label. When
	// SessionID is empty or "new" and Name is set, the daemon does
	// a "create-if-missing" attach — looks up an existing session
	// by name first, spawns a new one with that name if absent.
	// When SessionID is set explicitly, Name is ignored on
	// reattach (the session's identity is fixed at creation).
	// Names must be unique per daemon; collisions on a fresh
	// spawn return ErrNameInUse.
	Name string `cbor:"name,omitempty"`

	// Persist is the tri-state opt-in for cross-restart session
	// persistence:
	//   nil   → use the daemon-wide default
	//          (`mtroamd serve --persistence-default`, default on).
	//   *true  → explicitly opt this session in.
	//   *false → explicitly opt this session out.
	//
	// Pointer-to-bool is the wire-side encoding of the three states
	// (CBOR omitempty drops nil; serialises true/false explicitly).
	// This lets clients that don't care about persistence inherit
	// the daemon's policy without having to know its value, and
	// older clients that don't set the field round-trip cleanly as
	// "use default."
	//
	// Resolved through Registry.ResolvePersist into a concrete bool
	// at session-spawn time. Ignored on reattach to an existing
	// session — persistence is fixed at spawn; opt-out a running
	// session by killing + respawning.
	Persist *bool `cbor:"p,omitempty"`

	// Kind, when non-empty, routes this allocate to a registered
	// AllocateExtension (see daemon.AllocateExtension) instead of the core
	// PTY-shell path. The core never interprets it beyond matching the
	// registered Kind; a stock terminal daemon registers none, so any Kind is
	// rejected. ExtBody is an opaque CBOR payload whose schema is owned by the
	// extension that handles Kind.
	Kind    string `cbor:"kind,omitempty"`
	ExtBody []byte `cbor:"ext,omitempty"`

	// Env is optional extra environment for a NEW session's shell,
	// merged over the daemon's curated per-session vars (a KEY here
	// wins over the daemon's MESHTERM_* only if it collides, which it
	// never should). Populated by `mtroamd connect --env-file`, which
	// reads and deletes a 0600 file the iOS client SFTP-staged, so
	// values never appear in the connect process's argv. Ignored on
	// reattach (env can't apply to an already-running shell) and not
	// persisted to session meta (a cross-restart lazy respawn comes up
	// without it, matching attach semantics). Additive/omitempty: older
	// daemons decoding a newer client's request drop this key via
	// StrictDecMode; older clients simply never set it.
	Env map[string]string `cbor:"env,omitempty"`
}

// AllocateResponse carries the fields that go into the bootstrap
// line printed to stdout. Ok=false means the request failed; Err
// describes the failure.
type AllocateResponse struct {
	T  string `cbor:"t"`
	Ok bool   `cbor:"ok"`

	// On success:
	SessionID   string `cbor:"sid,omitempty"`  // 32 hex chars
	AttachToken string `cbor:"tok,omitempty"`  // 32 hex chars, single-use, 30s TTL
	Port        uint16 `cbor:"port,omitempty"` // QUIC UDP port
	// TCPPort is the plain-TCP mtRoam listener's bound port, populated
	// when the daemon was started with --mtroam-tcp-addr. Surfaced on
	// the bootstrap line as MTRM_TCP so the iOS client can dial the
	// daemon over its in-process tsnet (embedded-Tailscale mode).
	// Omitted (0) when the TCP listener is disabled — clients with
	// embedded mode enabled then surface "host needs daemon update"
	// to the user.
	TCPPort         uint16 `cbor:"tcp_port,omitempty"`
	LoopbackTCPPort uint16 `cbor:"loopback_tcp_port,omitempty"`
	CertFP          string `cbor:"cert_fp,omitempty"` // 64 hex chars, SHA-256 of cert DER
	// Name is the resolved user-visible session label. Echoed back
	// so the client knows what the daemon synthesised when the
	// request didn't supply one (see ipc/types.go AllocateRequest.Name).
	Name string `cbor:"name,omitempty"`

	// Reused reports whether this allocate resolved to an ALREADY-RUNNING
	// session (true: by-id lookup or create-by-name that found a live
	// session) or spawned a fresh shell (false). Pointer so absence is
	// distinguishable from false: an older daemon simply never sets it,
	// and `mtroamd connect` only emits the MTRM_SESSION_REUSED bootstrap
	// line when it's present. iOS uses this to decide whether the shell
	// already received --env-file at spawn (fresh) or needs a live
	// injection pass (reused) - see the secret-profiles delivery flow.
	// Additive/omitempty like Env: old peers drop the unknown key.
	Reused *bool `cbor:"reused,omitempty"`

	// HookInstalled reports whether the session's shell has a working
	// live-inject prompt hook seeded by the sidecar at spawn (the shim
	// that auto-sources ~/.mt-inject-<sessionID> on each prompt). Like
	// Reused, it's a pointer so absence is distinguishable from false:
	// an older daemon never sets it (nil = unknown), *true = a bash/zsh
	// hook is installed and will fire, *false = no working hook
	// (dash/sh, an unknown shell, or a seeding failure). `mtroamd
	// connect` emits the MTRM_LIVE_INJECT bootstrap line only when it's
	// present. iOS uses it to decide whether SFTPing a per-session drop
	// file will actually be sourced (hook present) or whether it must
	// fall back to another injection path. nil for extension allocates
	// (the extension owns its own spawn). Additive/omitempty like
	// Reused: old peers drop the unknown key.
	HookInstalled *bool `cbor:"hook,omitempty"`

	// BootID identifies THIS DAEMON PROCESS instance (random hex,
	// regenerated every daemon start; not persisted). `mtroamd connect`
	// emits it as the MTRM_BOOT_ID bootstrap line. iOS keys its
	// "already delivered broker secrets" cache on (boot id, content
	// fingerprint): the secret store is RAM-only, so a changed boot id
	// means the store was wiped and a re-push is required, while an
	// unchanged one lets a reconnect skip the redundant SFTP stage +
	// set-secrets exec entirely (review finding — every .ready re-pushed
	// over two extra connections even when the daemon already held the
	// set). Additive/omitempty: old peers drop the unknown key.
	BootID string `cbor:"boot_id,omitempty"`

	// ShimReady reports whether the broker shim dir is guaranteed FIRST on
	// the session shell's live PATH, so a brokered `set-secrets` would
	// actually reach its tools. Pointer: nil = unknown (pre-broker spawn) →
	// the client warns the user to regenerate the session; *true = ready.
	// `mtroamd connect` emits it as MTRM_SHIM_READY.
	//
	// ★★ FIRST, not merely present, and it is a property of the SHELL not of
	// the spawn. Being on PATH at spawn proves nothing: the user's rc runs
	// afterwards and an ordinary `PATH="$HOME/bin:$PATH"` outranks it.
	//
	// ★★ As of v1.7.11 this is an OBSERVATION, not a prediction. True means the
	// session's shell itself reported, from its own prompt, that it found the
	// shim dir first on its live PATH. Earlier versions computed it at seed time
	// from whether the shell was a login shell, which was a guess about what the
	// user's rc would do and was wrong in at least six ways that all reported
	// ready while the shim was outranked or absent. The daemon re-reads it live
	// on every allocate. See ptysidecar.ShimStatusFilename.
	// Additive/omitempty like Reused: old peers drop the unknown key.
	ShimReady *bool `cbor:"shim_ready,omitempty"`

	// ShimNotReadyReason explains a non-true ShimReady so the client can say
	// something the user can act on. Empty when ShimReady is *true.
	//
	// The single bool could not carry this: the client rendered every false as
	// "regenerate the session", which is right for a stale pre-broker session and
	// useless for a fish or nushell user, since regenerating never seeds those
	// shells. Those users got the same dead-end warning on every allocate forever.
	//
	// Values (additive; treat an unrecognised value as ShimNotReadyUnknown):
	//   - "unsupported_shell" - we never seeded this shell (fish/nushell/csh, or
	//     a `-c` invocation). Regenerating will NOT help; the user must change
	//     their login shell. Do not tell them to regenerate.
	//   - "awaiting_prompt"   - seeded, but the shell has not yet reported from a
	//     prompt. Either it genuinely has not reached one, or a startup file
	//     `exec`ed away before it could. Transient for a normal shell.
	//   - "unknown"           - pre-broker session, or the status could not be
	//     read. Regenerating IS the right advice here.
	ShimNotReadyReason string `cbor:"shim_not_ready_reason,omitempty"`

	// On failure:
	Err string `cbor:"err,omitempty"`
	Msg string `cbor:"msg,omitempty"`
}

// SessionInfo is one row of the ListSessionsResponse inventory.
// Used both over CBOR (IPC response shape) and over JSON (the
// `mtroamd list --json` output that iOS consumes via SSH);
// field tags are short stable strings so CBOR + JSON tooling
// produce a consistent wire form.
//
// AttachedNow is preserved as a bool for backwards-compat with
// pre-multi-attach iOS clients (decoding via Codable's
// decodeIfPresent on the field). AttachedModes is the richer
// multi-attach view: one entry per attached client, value is
// "exclusive" or "readonly". Equivalent to len(AttachedModes) > 0
// for AttachedNow's purposes; emitted by daemons that know about
// the field, ignored by older clients that don't.
type SessionInfo struct {
	ID             string   `cbor:"sid" json:"id"`
	Name           string   `cbor:"name" json:"name"`
	CreatedAtNs    int64    `cbor:"cn" json:"created_at_ns"`
	LastActiveAtNs int64    `cbor:"la" json:"last_active_at_ns"`
	AttachedNow    bool     `cbor:"att" json:"attached_now"`
	AttachedModes  []string `cbor:"am,omitempty" json:"attached_modes,omitempty"`
	IdleTimeoutNs  int64    `cbor:"itn,omitempty" json:"idle_timeout_ns,omitempty"`
	Rows           uint16   `cbor:"rows,omitempty" json:"rows,omitempty"`
	Cols           uint16   `cbor:"cols,omitempty" json:"cols,omitempty"`

	// Fg is the session PTY's current foreground command name
	// ("claude", "codex", "vim", …) — kernel truth from the sidecar's
	// tcgetpgrp poller, ≤5s fresh. v1.6.1+ field, optional so older
	// daemons/clients round-trip cleanly. Empty/absent = unknown.
	// Raw process NAME only (no arguments) — agent taxonomy stays
	// client-side.
	Fg string `cbor:"fg,omitempty" json:"fg,omitempty"`

	// Labels are generic client-facing key/value metadata an embedder attached
	// to the session (e.g. "kind"=agent, "interactive"=1), so a client can
	// categorise sessions in its picker. Opaque to the core; optional, so older
	// daemons/clients round-trip cleanly.
	Labels map[string]string `cbor:"labels,omitempty" json:"labels,omitempty"`

	// Wedge-watcher cumulative counters. Optional so older daemon
	// builds (pre-v0.9.4) can round-trip with newer mtroam clients
	// without protocol breakage. Populated for every live session;
	// zero values are valid and indicate "no wedge events for this
	// session yet". Surfaces in `mtroamd session-info` / `mtroam
	// session-info` so operators can correlate session size + age
	// with wedge frequency without grepping the JSONL.
	WedgeTotalOutBytes      uint64 `cbor:"wto,omitempty" json:"wedge_total_out_bytes,omitempty"`
	WedgeResizesObserved    uint64 `cbor:"wro,omitempty" json:"wedge_resizes_observed,omitempty"`
	WedgeSilentWedges       uint64 `cbor:"wsw,omitempty" json:"wedge_silent,omitempty"`
	WedgeCursorWedges       uint64 `cbor:"wcw,omitempty" json:"wedge_cursor_row,omitempty"`
	WedgeVerticalWalkWedges uint64 `cbor:"wvw,omitempty" json:"wedge_vertical_walk,omitempty"`
}

// ListSessionsRequest enumerates every live session on the daemon.
// No filter — the SSH boundary is the auth boundary; if you can
// reach the IPC socket you can see everything.
type ListSessionsRequest struct {
	T string `cbor:"t"`
}

// ListSessionsResponse returns the inventory snapshot. Ok=false
// indicates an internal failure; the snapshot is empty in that case.
type ListSessionsResponse struct {
	T        string        `cbor:"t"`
	Ok       bool          `cbor:"ok"`
	Sessions []SessionInfo `cbor:"sessions,omitempty"`
	Err      string        `cbor:"err,omitempty"`
	Msg      string        `cbor:"msg,omitempty"`
}

// KillSessionRequest reaps a session by ID or name. Sel is the
// selector — tried as a hex SessionID first, falls back to a name
// lookup on parse failure. Single-arg form keeps the CLI surface
// simple (`mtroamd kill <id-or-name>`).
type KillSessionRequest struct {
	T   string `cbor:"t"`
	Sel string `cbor:"sel"`
}

// KillSessionResponse mirrors AllocateResponse's success/failure
// shape. ErrUnknownSession when the selector resolves to nothing.
type KillSessionResponse struct {
	T   string `cbor:"t"`
	Ok  bool   `cbor:"ok"`
	Err string `cbor:"err,omitempty"`
	Msg string `cbor:"msg,omitempty"`
}

// RenameSessionRequest changes a session's user-visible Name. Sel
// follows the same id-or-name resolution as KillSession.
// The PTY + ring buffer + active attach are untouched — this is a
// pure-label change. Empty NewName is rejected (anonymous-by-rename
// would leave a session unreachable via the picker).
type RenameSessionRequest struct {
	T       string `cbor:"t"`
	Sel     string `cbor:"sel"`
	NewName string `cbor:"new"`
}

// RenameSessionResponse echoes the new name on success so the
// caller can confirm the daemon's view matches.
type RenameSessionResponse struct {
	T    string `cbor:"t"`
	Ok   bool   `cbor:"ok"`
	Name string `cbor:"name,omitempty"`
	Err  string `cbor:"err,omitempty"`
	Msg  string `cbor:"msg,omitempty"`
}

// StatusRequest asks the daemon for its current operational
// snapshot. Read-only; no parameters. Useful for health probes
// (Phase 5 install flow + systemd unit health checks) and for
// debugging "is the daemon I think I'm talking to actually the
// daemon I'm talking to?"
type StatusRequest struct {
	T string `cbor:"t"`
}

// StatusResponse carries one snapshot of the daemon's
// configuration + live counters. Field tags are short stable
// strings; JSON tags match the wire shape `mtroamd status
// --json` emits for tooling consumers.
type StatusResponse struct {
	T  string `cbor:"t" json:"-"`
	Ok bool   `cbor:"ok" json:"ok"`

	Version     string `cbor:"ver,omitempty" json:"version"`
	StartedAtNs int64  `cbor:"sat,omitempty" json:"started_at_ns"`
	UptimeNs    int64  `cbor:"upt,omitempty" json:"uptime_ns"`
	QUICAddr    string `cbor:"qa,omitempty" json:"quic_addr"`
	// MTRoamTCPAddr is the plain-TCP mtRoam listener's bound address,
	// surfaced when the daemon is started with --mtroam-tcp-addr.
	// Empty when the TCP listener is disabled (the default —
	// daemon ships QUIC-only). iOS clients in embedded-Tailscale
	// mode use this to dial the daemon via tsnet; system / direct
	// mode clients ignore it and use QUICAddr.
	MTRoamTCPAddr    string `cbor:"rta,omitempty" json:"mtroam_tcp_addr,omitempty"`
	CertFingerprint  string `cbor:"fp,omitempty" json:"cert_fingerprint"`
	SessionCount     int    `cbor:"sc,omitempty" json:"session_count"`
	MaxSessions      int    `cbor:"ms,omitempty" json:"max_sessions"`
	IdleTimeoutNs    int64  `cbor:"itn,omitempty" json:"idle_timeout_ns"`
	MaxIdleTimeoutNs int64  `cbor:"mitn,omitempty" json:"max_idle_timeout_ns"`
	PendingTokens    int    `cbor:"pt,omitempty" json:"pending_tokens"`

	Err string `cbor:"err,omitempty" json:"err,omitempty"`
	Msg string `cbor:"msg,omitempty" json:"msg,omitempty"`
}

// PingRequest is a liveness check used by `mtroamd connect` to
// verify the daemon is alive before any session work. The daemon
// echoes a PingResponse with the same nonce.
type PingRequest struct {
	T     string `cbor:"t"`
	Nonce uint64 `cbor:"n"`
}

// PingResponse echoes a PingRequest's nonce.
type PingResponse struct {
	T     string `cbor:"t"`
	Nonce uint64 `cbor:"n"`
}

// SessionSearchRequest scans the named session's scrollback ring for
// regex matches. Sel follows the same id-or-name resolution as
// KillSession. Pattern is the raw Go regexp/RE2 source; the daemon
// compiles it. Anchored=true wraps the pattern with (?m) so ^/$
// match at physical newlines in the retained bytes (the truncated
// ring start is NOT treated as ^). MaxMatches caps result count;
// 0 → daemon default (10,000).
type SessionSearchRequest struct {
	T          string `cbor:"t"`
	Sel        string `cbor:"sel"`
	Pattern    string `cbor:"pat"`
	MaxMatches int    `cbor:"max,omitempty"`
	Anchored   bool   `cbor:"anc,omitempty"`
}

// SearchMatchInfo is one row in a SessionSearchResponse. Byte offsets
// are in the buffer's monotonic seq space, so the caller can ReadSince
// for surrounding context if more than the immediate line is wanted.
// LineNum is 0-based within the retained scrollback (not absolute
// across session history — the ring can't know lines that have aged
// out). JSON tags match the wire shape `mtroamd session-search
// --json` emits for tooling consumers.
type SearchMatchInfo struct {
	StartSeq uint64 `cbor:"ss" json:"start_seq"`
	EndSeq   uint64 `cbor:"es" json:"end_seq"`
	Line     string `cbor:"l"  json:"line"`
	LineNum  int    `cbor:"n"  json:"line_num"`
}

// SessionSearchResponse carries the regex hits. Ok=false on bad
// pattern, unknown session, or internal error; Matches is empty in
// that case. An empty Matches with Ok=true means "valid request, no
// matches found."
type SessionSearchResponse struct {
	T       string            `cbor:"t" json:"-"`
	Ok      bool              `cbor:"ok" json:"ok"`
	Matches []SearchMatchInfo `cbor:"m,omitempty" json:"matches,omitempty"`
	Err     string            `cbor:"err,omitempty" json:"err,omitempty"`
	Msg     string            `cbor:"msg,omitempty" json:"msg,omitempty"`
}

// Reasons for a non-true AllocateResponse.ShimReady. Wire-stable strings.
// Additive: a client must treat an unrecognised value as ShimNotReadyUnknown
// rather than assuming the set is closed.
const (
	// ShimNotReadyUnsupportedShell: never seeded (fish/nushell/csh, or a `-c`
	// invocation). ★ Regenerating the session does NOT help - the client must not
	// offer that as the fix here, which is precisely what the old bare bool made
	// it do.
	ShimNotReadyUnsupportedShell = "unsupported_shell"
	// ShimNotReadyAwaitingPrompt: seeded, but the shell has not reported from a
	// prompt yet. Transient for a healthy shell; permanent if a startup file
	// `exec`ed away before the first prompt.
	ShimNotReadyAwaitingPrompt = "awaiting_prompt"
	// ShimNotReadyUnknown: pre-broker session or unreadable status. Regenerating
	// IS the right advice.
	ShimNotReadyUnknown = "unknown"
)

// Error codes used in AllocateResponse.Err. Wire-stable strings.
const (
	ErrUnknownSession = "unknown_session"
	ErrCapacity       = "capacity"
	ErrSpawnFailed    = "spawn_failed"
	ErrInternal       = "internal"
	ErrBadRequest     = "bad_request"
	ErrNameInUse      = "name_in_use"
)
