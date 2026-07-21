// Package secret implements mtroamd's in-memory secret broker with
// value-hiding delivery. Secrets are pushed per-session by the client
// (SetSessionSecrets), held ONLY in daemon memory (never on disk, never
// in the persisted session snapshot), and delivered to consuming
// commands at exec time via PATH-shadow shims + `mtroamd secret-exec`.
// The value is injected into the consuming tool's environment and never
// appears in the agent's own environment, in argv (`ps`), or in stdout
// the agent/model sees — the "nopeek" property, native and dependency-free.
//
// This package is the pure core (parse + store + shim rendering +
// real-binary resolution). The IPC transport, the `secret-exec` /
// `set-secrets` subcommands, and the spawn-time PATH wiring consume it.
package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Entry is one secret: a POSIX env-var name, its value, and the command
// BASENAMES it is injected into. Cmds empty means "the caller's default
// set" — the broker does not invent a default; the client resolves it
// before sending, so the wire form is always explicit.
//
// This is THE shared wire shape between the iOS client and the daemon:
// the client encodes a Payload to JSON, and `mtroamd set-secrets` parses
// it here. Keep the JSON tags (`key`,`value`,`cmds`) in lockstep with
// the Swift encoder (SecretBrokerPayload).
type Entry struct {
	Key   string   `json:"key"`
	Value string   `json:"value"`
	Cmds  []string `json:"cmds"`
}

// Payload is a full SetSessionSecrets body: the complete secret set for
// a session (not a delta — a push REPLACES the session's set, so a
// removed key simply stops appearing). Encoded to JSON by the client,
// staged 0600, read+deleted by `mtroamd set-secrets`.
type Payload struct {
	Secrets []Entry `json:"secrets"`
}

// ParsePayload decodes and VALIDATES a SetSessionSecrets JSON body.
// Every key must be a POSIX env-var name and every command a safe
// basename (they become shim filenames and are exec'd), so both are
// re-checked here regardless of any client-side validation — this is
// the trust boundary. An invalid key/command fails the whole payload
// rather than silently dropping, so the client learns its bug.
func ParsePayload(data []byte) (Payload, error) {
	var p Payload
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Payload{}, fmt.Errorf("secret: decode payload: %w", err)
	}
	seen := make(map[string]struct{}, len(p.Secrets))
	for i, e := range p.Secrets {
		if !ValidKey(e.Key) {
			return Payload{}, fmt.Errorf("secret: entry %d: invalid key %q", i, e.Key)
		}
		if _, dup := seen[e.Key]; dup {
			return Payload{}, fmt.Errorf("secret: duplicate key %q", e.Key)
		}
		seen[e.Key] = struct{}{}
		for _, c := range e.Cmds {
			if !ValidCommand(c) {
				return Payload{}, fmt.Errorf("secret: entry %d (%s): invalid command %q", i, e.Key, c)
			}
		}
	}
	return p, nil
}

// ValidKey reports whether s is a POSIX env-var name: [A-Za-z_][A-Za-z0-9_]*.
func ValidKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidCommand reports whether c is a safe command BASENAME. These
// become filenames in the shim dir and are exec'd, so: non-empty, ≤64
// bytes, not "."/"..", and limited to [A-Za-z0-9._-] (no slashes, no
// shell metacharacters, no whitespace). Matches the Swift
// SecretProfile.isValidCommand charset exactly.
func ValidCommand(c string) bool {
	if c == "" || len(c) > 64 || c == "." || c == ".." {
		return false
	}
	for _, r := range c {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// Store holds each session's secret set in memory. Never persisted:
// the daemon's session snapshotter must not reach into this. A session
// close / GC drops its entry; a daemon restart loses the whole map
// (the client re-pushes on reconnect), which is the intended trade for
// zero on-disk secret footprint.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]Payload // sid → its full secret set
}

// NewStore returns an empty broker store.
func NewStore() *Store {
	return &Store{sessions: make(map[string]Payload)}
}

// SetSession REPLACES the session's secret set (SetSessionSecrets is a
// full push, not a delta). An empty payload clears the session, same as
// ClearSession — a rotation that removes a key stops delivering it on
// the next exec, no restart.
func (s *Store) SetSession(sid string, p Payload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p.Secrets) == 0 {
		delete(s.sessions, sid)
		return
	}
	s.sessions[sid] = p
}

// ClearSession drops a session's secrets (session close / GC).
func (s *Store) ClearSession(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sid)
}

// EnvForCommand returns the KEY=value map a given command should be
// exec'd with — every entry whose Cmds list includes `command`. Empty
// (nil) when the session has no secrets for that command. This is the
// ONLY read path a consuming tool gets: it receives values for the
// command it named, nothing else, and never the full set.
func (s *Store) EnvForCommand(sid, command string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.sessions[sid]
	if !ok {
		return nil
	}
	var env map[string]string
	for _, e := range p.Secrets {
		for _, c := range e.Cmds {
			if c == command {
				if env == nil {
					env = make(map[string]string)
				}
				env[e.Key] = e.Value
				break
			}
		}
	}
	return env
}

// Commands returns the sorted union of every command any of the
// session's secrets declares — the set of shims that need to exist.
func (s *Store) Commands(sid string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.sessions[sid]
	if !ok {
		return nil
	}
	set := make(map[string]struct{})
	for _, e := range p.Secrets {
		for _, c := range e.Cmds {
			set[c] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ShimScript renders the shim for one command: a pure `exec` into
// `mtroamd secret-exec --shim-dir <dir> <command> -- "$@"`, which injects
// the command's secrets and execs the real binary.
//
//   - `exec` (not a subprocess): secret-exec replaces THIS shim process,
//     so the command runs exactly once. An earlier "run secret-exec then
//     fall back" form double-executed the command (secret-exec ran it as a
//     subprocess, returned, then the fallback ran it again) - caught in
//     device testing; destructive for `git push` / `gh pr create`.
//   - `--shim-dir` passes the shim dir EXPLICITLY (not just via the
//     MESHTERM_SHIM_DIR env), so secret-exec always knows which dir to
//     skip and can never resolve a command back to its own shim (the
//     self-exec loop when the env var is cleared).
//
// Fail-open lives in secret-exec (it execs the real command even when it
// can't fetch secrets), so under ANY current daemon a tool never breaks.
// KNOWN limitation: if the daemon BINARY at this path is DOWNGRADED below
// the broker version while a session persists with these shims, the old
// binary rejects `secret-exec` (exit 2) and the shimmed command breaks
// until the session is recreated. Accepted: rollback below the broker tag
// is a rare explicit operator action, and a shell fallback to cover it
// reintroduced the far-worse double-execution above.
//
// `daemonBinary`, `shimDir`, `socketPath` and `command` are our own
// controlled values (absolute paths / ValidCommand basename),
// single-quoted defensively.
//
// `--socket` pins the DAEMON'S OWN socket path into the shim: secret-exec
// runs inside the session's curated env (pty.BuildEnv allowlist), which
// strips XDG_RUNTIME_DIR - so path DISCOVERY there would miss a daemon
// bound to $XDG_RUNTIME_DIR/mtroamd.sock and silently fail open on every
// command (tool runs token-less with no error; review finding). The
// daemon knows exactly where it is listening; the shim should never have
// to guess. Empty socketPath (tests / non-daemon callers) omits the flag
// and secret-exec falls back to discovery.
func ShimScript(daemonBinary, shimDir, socketPath, command string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	sock := ""
	if socketPath != "" {
		sock = " --socket " + q(socketPath)
	}
	return "#!/bin/sh\nexec " + q(daemonBinary) + " secret-exec --shim-dir " +
		q(shimDir) + sock + " " + q(command) + ` -- "$@"` + "\n"
}

// SyncShims makes `dir` contain EXACTLY one 0755 shim per command in
// `commands` and removes any stale shim (a command dropped from the
// set). Creates dir (0700) if missing. Called after every SetSession so
// a mid-session command-list change adds/removes shims live — PATH
// already includes dir (set at spawn), so a newly-written shim is found
// on the next lookup with no restart.
func SyncShims(dir, daemonBinary, socketPath string, commands []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secret: mkdir shim dir: %w", err)
	}
	want := make(map[string]struct{}, len(commands))
	for _, c := range commands {
		if !ValidCommand(c) {
			return fmt.Errorf("secret: refusing shim for invalid command %q", c)
		}
		want[c] = struct{}{}
		path := filepath.Join(dir, c)
		if err := os.WriteFile(path, []byte(ShimScript(daemonBinary, dir, socketPath, c)), 0o755); err != nil {
			return fmt.Errorf("secret: write shim %s: %w", c, err)
		}
	}
	// Prune stale shims (dropped commands). Best-effort: a leftover shim
	// is harmless (secret-exec fail-opens), so a remove error only logs
	// upstream, never fails the sync.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if _, keep := want[ent.Name()]; !keep {
			_ = os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
	return nil
}

// ResolveReal finds the real executable for `command` by scanning
// pathEnv (a colon-separated PATH) and skipping `shimDir` — so the
// shim never re-resolves to itself. Returns the first executable match.
// Empty shimDir scans the whole PATH (used when no shadowing is active).
func ResolveReal(command, shimDir, pathEnv string) (string, error) {
	if strings.ContainsRune(command, os.PathSeparator) {
		// An explicit path was given; honor it verbatim.
		return command, nil
	}
	// Clean the shim dir ONLY when non-empty: filepath.Clean("") == ".",
	// which would otherwise make the skip below match a "." PATH entry (or,
	// worse, disable the skip and let a command resolve back to its own
	// shim - the self-exec loop). Empty shimDir = "skip nothing"; the shim
	// always passes a real --shim-dir, so the empty case is only a direct
	// `secret-exec` invocation with no shim on PATH.
	var cleanShim string
	if shimDir != "" {
		cleanShim = filepath.Clean(shimDir)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		if cleanShim != "" && filepath.Clean(dir) == cleanShim {
			continue // don't resolve back into the shim dir
		}
		candidate := filepath.Join(dir, command)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("secret: %q not found on PATH (excluding shim dir)", command)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
