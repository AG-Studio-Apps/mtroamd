package ptysidecar

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// HookStatusFilename is the per-session file the sidecar writes (in
// the session dir, next to sidecar.sock) recording whether it seeded a
// working live-inject prompt hook: "1" (installed) or "0" (not). The
// daemon-side ptyclient reads it after a successful dial to learn the
// value the detached sidecar computed. Written BEFORE the sidecar binds
// its listener, so a successful dial implies the file is present.
const HookStatusFilename = "hook-installed"

// hookrcSubdir is the per-session directory (under the session dir)
// holding the temp rc files the sidecar generates to chain the user's
// real rc + the live-inject hook. Kept for the session's lifetime: a
// zsh child inherits ZDOTDIR pointing here and re-reads these on start.
const hookrcSubdir = "hookrc"

// promptHookBody is the exact shell snippet seeded after the user's rc.
// It defines _mt_inject (sources + removes ~/.mt-inject-$MT_TOK on each
// prompt) and wires it into precmd_functions (zsh) or PROMPT_COMMAND
// (bash). BYTE-IDENTICAL to what iOS expects: the .mt-inject- prefix and
// the MT_TOK variable are part of the contract. Do not reformat.
const promptHookBody = `command -v _mt_inject >/dev/null 2>&1 || { export MT_TOK="$MESHTERM_SESSION_ID"; _mt_inject(){ [ -n "$MT_TOK" ] || return; f="$HOME/.mt-inject-$MT_TOK"; [ -f "$f" ] && { . "$f"; rm -f "$f"; }; }; if [ -n "$ZSH_VERSION" ]; then precmd_functions+=(_mt_inject); else PROMPT_COMMAND="${PROMPT_COMMAND:+$PROMPT_COMMAND; }_mt_inject"; fi; }`

// shellKind classifies a shell by its basename for hook seeding.
type shellKind int

const (
	shellUnknown shellKind = iota
	shellBash
	shellZsh
	shellPosix // sh / dash — POSIX $ENV, no prompt hook
)

func classifyShell(shellPath string) shellKind {
	switch filepath.Base(shellPath) {
	case "bash":
		return shellBash
	case "zsh":
		return shellZsh
	case "sh", "dash":
		return shellPosix
	default:
		return shellUnknown
	}
}

// seedResult is what seedPromptHook hands back: the (possibly rewritten)
// shell invocation + environment, and whether a working prompt hook was
// installed. On any failure the original invocation is returned verbatim
// with hookInstalled=false — a broken shell is far worse than no
// live-inject, so seeding never fails the spawn.
type seedResult struct {
	shell         string
	args          []string
	env           []string
	hookInstalled bool
}

// seedPromptHook installs the live-inject prompt hook for a session's
// shell, after the user's own rc so a user PROMPT_COMMAND/precmd can't
// clobber it. It returns the invocation the sidecar should exec. It is
// fail-safe: every error path returns the untouched original.
func seedPromptHook(sessionDir, shell string, args, env []string, log *slog.Logger) seedResult {
	orig := seedResult{shell: shell, args: args, env: env, hookInstalled: false}
	switch classifyShell(shell) {
	case shellBash:
		return seedBash(sessionDir, shell, args, env, log, orig)
	case shellZsh:
		return seedZsh(sessionDir, shell, args, env, log, orig)
	case shellPosix:
		// sh / dash have no per-prompt hook (no PROMPT_COMMAND /
		// precmd), so the live-inject shim can never fire there. We
		// deliberately do NOT seed a $ENV file either: the shared hook
		// body contains zsh array syntax (precmd_functions+=(...)) that
		// dash rejects at PARSE time, not just at runtime, so sourcing
		// it via $ENV would print a syntax error on every interactive
		// start. Leaving the shell untouched (hookInstalled=false) is
		// the fail-safe choice; iOS falls back for these shells.
		return orig
	default:
		// Unknown shell: no reliable prompt hook, leave it alone.
		return orig
	}
}

// classifyShellArgs reports whether the shell's args are limited to
// benign login/interactive flags (so we can safely rewrite the
// invocation), and whether a login shell was requested. Any arg that is
// not a recognized -l/--login/-i flag (a -c command, a script path, an
// unknown flag) makes benign=false: the shell is running a custom
// command, not a plain interactive shell, so we skip seeding entirely.
func classifyShellArgs(args []string) (benign, login bool) {
	for _, a := range args {
		switch {
		case a == "--login":
			login = true
		case strings.HasPrefix(a, "--"):
			return false, login // unknown long option
		case len(a) > 1 && a[0] == '-':
			// Short-flag cluster; permit only l (login) and i
			// (interactive) characters.
			for _, c := range a[1:] {
				switch c {
				case 'l':
					login = true
				case 'i':
					// interactive; harmless
				default:
					return false, login
				}
			}
		default:
			// A bare word (command / script path / "-").
			return false, login
		}
	}
	return true, login
}

// makeHookrcDir creates the per-session hookrc dir (0700).
func makeHookrcDir(sessionDir string) (string, error) {
	dir := filepath.Join(sessionDir, hookrcSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// seedBash installs the hook for bash. bash honors --rcfile only for
// INTERACTIVE NON-LOGIN shells, so we always launch bash non-login via a
// generated rcfile and source the appropriate user files inside it:
//   - non-login: source ~/.bashrc
//   - login: source /etc/profile + the first of
//     ~/.bash_profile / ~/.bash_login / ~/.profile (bash's login chain)
//
// then append the hook LAST. A PTY-attached bash with no command arg is
// interactive by default, so dropping the original -l/-i flags in favour
// of --rcfile keeps it interactive while making --rcfile take effect.
func seedBash(sessionDir, shell string, args, env []string, log *slog.Logger, orig seedResult) seedResult {
	benign, login := classifyShellArgs(args)
	if !benign {
		return orig
	}
	dir, err := makeHookrcDir(sessionDir)
	if err != nil {
		log.Warn("sidecar.hookseed.mkdir_failed", "shell", "bash", "err", err.Error())
		return orig
	}
	rcPath := filepath.Join(dir, "bashrc")

	var b strings.Builder
	b.WriteString("# meshTerm live-inject shim (bash)\n")
	if login {
		b.WriteString(`[ -r /etc/profile ] && . /etc/profile` + "\n")
		b.WriteString(`if [ -r "$HOME/.bash_profile" ]; then . "$HOME/.bash_profile"; elif [ -r "$HOME/.bash_login" ]; then . "$HOME/.bash_login"; elif [ -r "$HOME/.profile" ]; then . "$HOME/.profile"; fi` + "\n")
	} else {
		// A non-login interactive bash normally reads the system-wide
		// /etc/bash.bashrc (Debian/Ubuntu compile it as SYS_BASHRC) BEFORE
		// ~/.bashrc, but --rcfile replaces BOTH. Source it first so bash
		// completion, the distro default prompt, etc. are not lost - this is
		// the DEFAULT spawn (ShellArgs is typically nil = non-login).
		b.WriteString(`[ -r /etc/bash.bashrc ] && . /etc/bash.bashrc` + "\n")
		b.WriteString(`[ -r "$HOME/.bashrc" ] && . "$HOME/.bashrc"` + "\n")
	}
	b.WriteString(promptHookBody + "\n")

	if err := os.WriteFile(rcPath, []byte(b.String()), 0o600); err != nil {
		log.Warn("sidecar.hookseed.write_failed", "shell", "bash", "err", err.Error())
		return orig
	}
	return seedResult{
		shell:         shell,
		args:          []string{"--rcfile", rcPath},
		env:           env,
		hookInstalled: true,
	}
}

// seedZsh installs the hook for zsh by pointing ZDOTDIR at a generated
// dir whose startup files source the user's real ones (from the original
// ZDOTDIR, or $HOME if unset), append the hook to .zshrc, then restore
// ZDOTDIR so child processes and a login .zlogin see the real value.
// Both login and non-login work: ZDOTDIR redirects every startup-file
// lookup, so we don't touch the shell's args (login stays login).
func seedZsh(sessionDir, shell string, args, env []string, log *slog.Logger, orig seedResult) seedResult {
	benign, _ := classifyShellArgs(args)
	if !benign {
		return orig
	}
	dir, err := makeHookrcDir(sessionDir)
	if err != nil {
		log.Warn("sidecar.hookseed.mkdir_failed", "shell", "zsh", "err", err.Error())
		return orig
	}

	realZ, hadZ := envLookup(env, "ZDOTDIR")
	// Shell expression for the user's real ZDOTDIR: a quoted literal
	// when it was set, else $HOME (the zsh default when ZDOTDIR is
	// unset). Note the daemon's env allowlist normally drops ZDOTDIR,
	// so the common path is $HOME.
	realExpr := `"$HOME"`
	if hadZ {
		realExpr = shSingleQuote(realZ)
	}
	tempQ := shSingleQuote(dir)

	// Restore statement placed at the end of .zshrc so children + a
	// login .zlogin (read after .zshrc) see the real ZDOTDIR again.
	restore := "unset ZDOTDIR\n"
	if hadZ {
		restore = "ZDOTDIR=" + shSingleQuote(realZ) + "; export ZDOTDIR\n"
	}

	// .zshenv (always read). Re-assert ZDOTDIR to our dir afterwards in
	// case the user's .zshenv set it, so .zshrc still comes from us.
	zshenv := "# meshTerm live-inject shim (zsh)\n" +
		"[ -r " + realExpr + "/.zshenv ] && . " + realExpr + "/.zshenv\n" +
		"ZDOTDIR=" + tempQ + "\n"
	// .zprofile (login stage, before .zshrc). Same re-assert.
	zprofile := "[ -r " + realExpr + "/.zprofile ] && . " + realExpr + "/.zprofile\n" +
		"ZDOTDIR=" + tempQ + "\n"
	// .zshrc (interactive). Source the user's rc, install the hook
	// AFTER it, then restore ZDOTDIR.
	zshrc := "[ -r " + realExpr + "/.zshrc ] && . " + realExpr + "/.zshrc\n" +
		promptHookBody + "\n" +
		restore

	for name, body := range map[string]string{
		".zshenv":   zshenv,
		".zprofile": zprofile,
		".zshrc":    zshrc,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			log.Warn("sidecar.hookseed.write_failed", "shell", "zsh", "file", name, "err", err.Error())
			return orig
		}
	}

	return seedResult{
		shell:         shell,
		args:          args,
		env:           envReplace(env, "ZDOTDIR", dir),
		hookInstalled: true,
	}
}

// writeHookStatus records the seeded/not-seeded state to the session
// dir so the spawning ptyclient can read it after dial. Best-effort:
// a write failure leaves the file absent → the client reads nil
// (unknown), which is the safe fallback.
func writeHookStatus(sessionDir string, installed bool, log *slog.Logger) {
	val := []byte("0")
	if installed {
		val = []byte("1")
	}
	path := filepath.Join(sessionDir, HookStatusFilename)
	if err := os.WriteFile(path, val, 0o600); err != nil {
		log.Warn("sidecar.hookseed.status_write_failed", "err", err.Error())
	}
}

// envLookup returns the value of key in a KEY=VAL env slice and whether
// it was present.
func envLookup(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}

// envReplace returns env with key set to val, replacing any existing
// entry (last-write-wins is avoided; we drop prior occurrences). The
// input slice is not mutated.
func envReplace(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+val)
}

// shSingleQuote wraps s in single quotes, escaping embedded single
// quotes, so it is a safe literal in a POSIX shell script.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
