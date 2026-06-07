//go:build linux

package ptysidecar

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// foregroundComm returns the command name of the PTY's current
// foreground process group — the same mechanism tmux uses for
// pane_current_command: tcgetpgrp on the master fd, then the group
// leader's /proc/<pgid>/comm (falling back to the cmdline argv[0]
// basename for interpreter wrappers whose comm is unhelpful, e.g.
// "node"-shebanged CLIs).
//
// Returns "" when unresolvable: fd error, no foreground group, the
// leader already exited, or /proc unreadable. Privacy: this reads
// process NAMES only — never arguments beyond the argv[0] basename,
// never terminal content.
func foregroundComm(master *os.File) string {
	pgid, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return ""
	}
	// Group leader's pid == pgid under shell job control. If the
	// leader is gone (group survives leaderless), report "" rather
	// than guessing from group members.
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgid))
	if err == nil {
		name := strings.TrimSpace(string(comm))
		if name != "" && !isInterpreterComm(name) {
			return capComm(name)
		}
	}
	// comm missing or an interpreter name — try argv[0]'s basename
	// so "node /usr/local/bin/claude" classifies as "claude".
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pgid))
	if err != nil || len(cmdline) == 0 {
		return ""
	}
	args := bytes.Split(cmdline, []byte{0})
	// Prefer the SCRIPT basename (first non-flag arg after an
	// interpreter argv[0]); else argv[0]'s basename.
	if len(args) >= 2 && isInterpreterComm(filepath.Base(string(args[0]))) {
		for _, a := range args[1:] {
			s := string(a)
			if s == "" || strings.HasPrefix(s, "-") {
				continue
			}
			return capComm(filepath.Base(s))
		}
	}
	return capComm(filepath.Base(string(args[0])))
}

// isInterpreterComm reports whether a comm value is a script
// interpreter whose name hides the actual tool (the cmdline fallback
// then looks at the script path instead).
func isInterpreterComm(name string) bool {
	switch name {
	case "node", "python", "python3", "ruby", "perl", "bun", "deno":
		return true
	}
	return false
}

// capComm sanitizes to valid UTF-8 and truncates to MaxFgCommBytes.
// Kernel task names are arbitrary bytes — an invalid-UTF-8 comm fed
// into a CBOR text string would make MarshalAttachAck/AgentNotify
// FAIL, turning a weird process name into an attach failure. The
// byte cap is re-sanitized so a rune split at the boundary is
// dropped rather than emitted torn.
func capComm(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > MaxFgCommBytes {
		s = strings.ToValidUTF8(s[:MaxFgCommBytes], "")
	}
	return s
}
