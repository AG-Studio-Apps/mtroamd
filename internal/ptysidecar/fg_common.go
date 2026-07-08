package ptysidecar

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"strings"
)

// Portable helpers shared by the per-platform fg resolvers
// (fg_linux.go, fg_darwin.go). Kept build-tag-free so the pure logic —
// especially the procargs decoder below — compiles and is unit-tested
// in CI regardless of host OS.

// capComm sanitizes to valid UTF-8 and truncates to MaxFgCommBytes.
// Kernel task names are arbitrary bytes — an invalid-UTF-8 comm fed
// into a CBOR text string would make MarshalAttachAck/AgentNotify
// FAIL, turning a weird process name into an attach failure. The byte
// cap is re-sanitized so a rune split at the boundary is dropped
// rather than emitted torn.
func capComm(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > MaxFgCommBytes {
		s = strings.ToValidUTF8(s[:MaxFgCommBytes], "")
	}
	return s
}

// isInterpreterComm reports whether a comm value is a script
// interpreter whose name hides the actual tool (the argv fallback then
// looks at the script path instead, so "node …/claude" → "claude").
func isInterpreterComm(name string) bool {
	switch name {
	case "node", "python", "python3", "ruby", "perl", "bun", "deno":
		return true
	}
	return false
}

// agentMarker recognizes a known coding-agent CLI from a single argv token (a
// path or bare name), mirroring the iOS client's TmuxForegroundParser.canonical
// so the mtRoam daemon classifies node-wrapped agents IDENTICALLY to the
// plain-SSH poller. It catches both `.../bin/claude` (basename / suffix) AND
// `.../@anthropic-ai/claude-code/cli.js` (marker substring); the latter is the
// case a plain basename would mis-resolve to "cli.js". "" when not an agent.
func agentMarker(s string) string {
	l := strings.ToLower(s)
	base := filepath.Base(l)
	switch {
	case strings.Contains(l, "claude-code") || base == "claude" || strings.HasSuffix(l, "/claude"):
		return "claude"
	case base == "codex" || strings.HasSuffix(l, "/codex") ||
		strings.Contains(l, "/codex/") || strings.Contains(l, "codex-cli"):
		return "codex"
	case base == "agy" || strings.HasSuffix(l, "/agy") || strings.Contains(l, "antigravity"):
		return "agy"
	}
	return ""
}

// commFromCString trims a fixed-size C char array (e.g. kinfo_proc
// P_comm) at its first NUL and strips surrounding whitespace.
func commFromCString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// commFromArgs picks a meaningful command name from an argv list: the
// script basename when argv[0] is an interpreter, else argv[0]'s
// basename. "" for empty input. Shared by the Linux
// (/proc/<pid>/cmdline) and darwin (KERN_PROCARGS2) resolvers — the
// argv SOURCE differs, the selection must not. Caller applies capComm.
func commFromArgs(args [][]byte) string {
	if len(args) == 0 || len(args[0]) == 0 {
		return ""
	}
	if len(args) >= 2 && isInterpreterComm(filepath.Base(string(args[0]))) {
		// A known agent wins first, recognized by marker in ANY arg, so a
		// node-installed `node .../@anthropic-ai/claude-code/cli.js` resolves to
		// "claude" instead of the script basename "cli.js" (the case the daemon
		// used to miss vs the iOS client, which already marker-matches).
		for _, a := range args[1:] {
			if m := agentMarker(string(a)); m != "" {
				return m
			}
		}
		// Otherwise find the script. Prefer a path-like arg (contains a
		// separator or has an extension) so a value that belongs to a preceding
		// option, e.g. the "utf8" in `python -X utf8 app.py`, does not get
		// mistaken for the script. Fall back to the first non-flag arg if none
		// look like a path.
		var firstNonFlag string
		for _, a := range args[1:] {
			s := string(a)
			if s == "" || strings.HasPrefix(s, "-") {
				continue
			}
			if strings.Contains(s, "/") || strings.Contains(filepath.Base(s), ".") {
				return filepath.Base(s)
			}
			if firstNonFlag == "" {
				firstNonFlag = s
			}
		}
		if firstNonFlag != "" {
			return filepath.Base(firstNonFlag)
		}
	}
	return filepath.Base(string(args[0]))
}

// parseProcArgs decodes a macOS KERN_PROCARGS2 buffer into argv.
// Layout: [int32 argc][exec_path \0][\0 padding][argv0 \0][argv1 \0]…
// then the environment (ignored). Pure + bounds-checked so it's
// unit-tested in CI on any OS; the sysctl call that produces the
// buffer lives in fg_darwin.go. Tolerant of truncation/malformed input
// — never panics, returns best-effort argv (possibly nil).
func parseProcArgs(buf []byte) [][]byte {
	if len(buf) < 4 {
		return nil
	}
	// argc is host-endian; all darwin targets (amd64, arm64) are LE.
	argc := int(int32(binary.LittleEndian.Uint32(buf[:4])))
	if argc <= 0 || argc > 1024 { // sanity bound vs a malformed length
		return nil
	}
	p := buf[4:]
	// Skip the executable path (NUL-terminated).
	if i := bytes.IndexByte(p, 0); i >= 0 {
		p = p[i+1:]
	} else {
		return nil
	}
	// Skip NUL padding between exec_path and argv[0].
	for len(p) > 0 && p[0] == 0 {
		p = p[1:]
	}
	out := make([][]byte, 0, argc)
	for len(out) < argc && len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			out = append(out, p) // truncated final arg — take what's there
			break
		}
		out = append(out, p[:i])
		p = p[i+1:]
	}
	return out
}
