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

// ShimStatusFilename is the per-session file carrying the secret-broker
// shim-readiness bit ("1"/"0"), surfaced as MTRM_SHIM_READY.
//
// ★★ The WRITER changed in v1.7.11-rc3, and it is the whole point of this
// release. The sidecar now only ever writes "0" here, at seed time. The "1" is
// written by the SHELL ITSELF, from `_mt_shim_announce`, and only after
// `_mt_shim_path` has observed the shim dir actually sitting first on the live
// PATH.
//
// Every earlier version computed this bit at SEED time from `!login`, i.e. it
// PREDICTED that the shell would end up with the shim first. rc1/rc2 tightened
// the prediction but kept it a prediction, and the review found six separate
// ways for it to be wrong while still reporting ready: an `exec` in the user's
// rc (the shell never reaches a prompt), `set -u` aborting the register line, an
// array PROMPT_COMMAND reordering the re-assert, a nested shell, a stale command
// hash, and a session carried across the v1.7.10 upgrade. All six produced the
// same silent failure - iOS reports a brokered secret `.delivered` while the tool
// launches with none.
//
// A prediction can be wrong. An observation made by the shell that will actually
// run the tool cannot be wrong in the same direction: a shell that never reaches
// a prompt never announces, so the bit stays "0" and iOS warns. That asymmetry
// is the fix; the individual shell-quoting bugs below are secondary.
//
// The daemon re-reads this file LIVE on every allocate (see readShimStatus in
// internal/daemon) rather than caching it at spawn, because the announce
// necessarily happens after the spawn has returned.
const ShimStatusFilename = "shim-ready"

// shimFuncDef defines _mt_shim_path, which moves the broker shim dir to the
// FRONT of PATH. Split into a function (from a one-liner) and called on EVERY
// PROMPT, because a one-shot rc-time re-assert is defeated by any per-prompt
// PATH mutator - direnv, mise, asdf, conda and nvm all hook PROMPT_COMMAND /
// precmd and re-prepend their own dir on the first prompt, after our rc line
// has already run. Measured: PATH is correct when the rcfile finishes, and the
// user's binary wins from prompt one onward.
//
// ★★ The strip does NOT pattern-match on the shim dir. An earlier version used
// `case` to detect and `${var%%pat}` to remove, which are two different pattern
// engines: under `shopt -s nocasematch` the case kept matching while the strip
// removed nothing, and `_mt_p` DOUBLED in size every iteration. An iteration
// counter did not fix that (64 doublings is 2^64 bytes); measurement did, by
// hanging. This walks PATH element by element and compares with string
// EQUALITY, so nocasematch, glob metacharacters in the shim path, and
// case-insensitive filesystems are all irrelevant. Verified in bash, zsh and
// dash across already-first, present-late, duplicated, substring collisions
// (/Storage, /S-old), empty elements, a trailing colon, a glob in the shim dir,
// and nocasematch.
//
// `local` keeps every temporary out of the user's namespace, which the previous
// one-liner could not do: it used a bare `_mt_p` and then `unset` it,
// destroying a same-named user variable, and only on one of its two paths.
//
// ★ _MT_SHIM_OFF is set BEFORE the assignment and cleared after it succeeds. On
// a host with `readonly PATH` the assignment aborts the function, so a flag set
// afterwards would never run and the shell would print an error EVERY prompt.
// This way it is attempted once and then stays off.
// ★ statusPath is baked in as a shell literal rather than read from an env var.
// The sidecar already knows the session dir, and a literal cannot be cleared,
// shadowed or exported away by the user's rc - which matters when the whole
// point of the write is to be trustworthy.
//
// ★ The fast path (`${PATH%%:*}`) is not just a speed fix. The full walk is
// quadratic in PATH length - measured 0.30 ms at 10 entries, 2.3 ms at 40,
// 11.4 ms at 100, 39 ms at 200 - and it ran on EVERY prompt even when the shim
// was already first, which is the overwhelmingly common case. A nix/conda/asdf
// user with a long PATH was paying ~39 ms of prompt latency for nothing.
//
// ★ `hash -r` / `rehash` after a successful change: the shim dir is on PATH from
// spawn but POPULATED LAZILY by SyncShims on the first SetSessionSecrets. A tool
// run before its shim file existed is hashed to the real binary and STAYS hashed
// there, so fixing PATH order alone does not redirect it. Measured.
func shimFuncDef(statusPath string) string {
	q := shSingleQuote(statusPath)
	// _mt_shim_announce records the OBSERVATION that the shim is first. Written
	// at most once per shell (_MT_SHIM_ANNOUNCED is deliberately not `local`, so
	// it persists across prompts). Failure is silent: a read-only session dir
	// leaves the bit "0", which is the fail-CLOSED direction.
	announce := `_mt_shim_announce() { [ -z "${_MT_SHIM_ANNOUNCED:-}" ] || return 0; _MT_SHIM_ANNOUNCED=1; { printf 1 > ` + q + `; } 2>/dev/null; return 0; }`
	path := `_mt_shim_path() { local _mt_new _mt_e _mt_rest; [ -n "${MESHTERM_SHIM_DIR:-}" ] || return 0; [ -z "${_MT_SHIM_OFF:-}" ] || return 0; if [ "$PATH" = "$MESHTERM_SHIM_DIR" ] || [ "${PATH%%:*}" = "$MESHTERM_SHIM_DIR" ]; then _mt_shim_announce; return 0; fi; _mt_new=; _mt_rest="$PATH"; while :; do _mt_e="${_mt_rest%%:*}"; [ "$_mt_e" = "$MESHTERM_SHIM_DIR" ] || _mt_new="$_mt_new:$_mt_e"; [ "$_mt_rest" = "${_mt_rest#*:}" ] && break; _mt_rest="${_mt_rest#*:}"; done; _mt_new="${_mt_new#:}"; _mt_new="$MESHTERM_SHIM_DIR${_mt_new:+:$_mt_new}"; [ "$PATH" = "$_mt_new" ] && { _mt_shim_announce; return 0; }; _MT_SHIM_OFF=1; { PATH="$_mt_new"; } 2>/dev/null; if [ "$PATH" = "$_mt_new" ]; then export PATH; unset _MT_SHIM_OFF; if [ -n "${ZSH_VERSION:-}" ]; then rehash 2>/dev/null; else hash -r 2>/dev/null; fi; _mt_shim_announce; fi; return 0; }`
	return announce + "\n" + path
}

// shimRegisterLine wires _mt_shim_path into the per-prompt hook and runs it
// once immediately, so the shell starts correct and STAYS correct.
//
// ★ Registered SEPARATELY rather than called from inside promptHookBody. That
// body is a shared contract with iOS, which seeds its own byte-comparable copy
// (AgentEnv.injectHookInstall) that does NOT define _mt_shim_path - so a call
// added there would become "command not found" on every prompt of an
// iOS-seeded session.
// ★ `${ZSH_VERSION:-}`, not `$ZSH_VERSION`. A user rc running `set -u` (or
// `setopt nounset`) turned the bare dereference into an "unbound variable" error
// that aborted the rest of this line, so _mt_shim_path was never registered -
// while the old seed-time bit still reported ready. Measured: the error printed
// into the user's terminal at every session start and PROMPT_COMMAND stayed
// unset. shimFuncDef beside it was already written with `${VAR:-}` guards
// throughout; this line was the one that was not.
//
// ★ bash 5.1+ allows PROMPT_COMMAND to be an ARRAY, and appending a scalar to an
// array variable lands in element [0]. Verified in bash 5.2: with
// PROMPT_COMMAND=(a b), the old scalar append produced
// ([0]="a; _mt_shim_path" [1]="b"), so a direnv/conda/mise hook registered with
// the modern `PROMPT_COMMAND+=(...)` form ran AFTER our re-assert on every
// prompt and re-prepended its own dir. That defeats the entire premise of
// re-asserting per prompt, so the array case has to append as an array element.
const shimRegisterLine = `if [ -n "${ZSH_VERSION:-}" ]; then precmd_functions+=(_mt_shim_path); elif [ -n "${BASH_VERSION:-}" ] && [ "$(declare -p PROMPT_COMMAND 2>/dev/null | cut -c1-11)" = "declare -a " ]; then PROMPT_COMMAND+=(_mt_shim_path); else PROMPT_COMMAND="${PROMPT_COMMAND:+$PROMPT_COMMAND; }_mt_shim_path"; fi; _mt_shim_path`

// hookrcSubdir is the per-session directory (under the session dir)
// holding the temp rc files the sidecar generates to chain the user's
// real rc + the live-inject hook.
//
// ★ Kept for the session's lifetime: a child CAN still inherit ZDOTDIR pointing
// here, so these files must not vanish underneath it.
//
// ★★ Two wrong versions of this comment preceded this one, which is why it is
// specific about what was measured. The original claimed children always
// re-read these files; I replaced it with the opposite absolute ("restored
// before any child can spawn"); both are wrong. What is true: only the
// generated .zshrc restores ZDOTDIR, and only as its LAST statement. So a child
// started AFTER .zshrc completes sees the user's real ZDOTDIR and does not
// re-read (measured: a counter in the generated .zshrc stayed at 1 across an
// inner `zsh -i`), while a child started from .zshenv, .zprofile, or from
// inside the user's own .zshrc still inherits ours and does re-read them.
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
	// ★★ There is deliberately NO shimReady field any more.
	//
	// It used to live here, computed from `!login` at seed time, and every
	// version of that computation was a PREDICTION about what the user's rc would
	// do next. Predictions about other people's shell configuration are wrong in
	// ways that are invisible from here: an `exec` in the rc, `set -u`, an array
	// PROMPT_COMMAND, a nested shell. Each one left the bit reporting ready while
	// the shim was outranked or absent.
	//
	// The bit is now written by `_mt_shim_announce` from inside the shell that
	// will actually run the tool, once `_mt_shim_path` has observed the shim
	// sitting first on the live PATH. Seeding therefore no longer has an opinion
	// about readiness - it just seeds, and the shell reports what happened.
	//
	// Keeping a seed-time field here at all would re-open the hole, because the
	// only honest value it could carry ("we wrote the lines") is exactly what
	// hookInstalled already means.
}

// seedPromptHook installs the live-inject prompt hook for a session's
// shell, after the user's own rc so a user PROMPT_COMMAND/precmd can't
// clobber it. It returns the invocation the sidecar should exec. It is
// fail-safe: every error path returns the untouched original.
func seedPromptHook(sessionDir, shell string, args, env []string, log *slog.Logger) seedResult {
	// An UNSEEDED shell gets no _mt_shim_path, so nothing re-asserts the shim
	// after the user's rc runs and we can guarantee nothing. seedBash/seedZsh
	// override this to true on success.
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
	b.WriteString(shimFuncDef(filepath.Join(sessionDir, ShimStatusFilename)) + "\n")
	b.WriteString(promptHookBody + "\n")
	b.WriteString(shimRegisterLine + "\n")

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
	// login is discarded: zsh seeding is identical either way (ZDOTDIR redirects
	// every startup-file lookup), and the login flag's only other use was the
	// seed-time shimReady prediction that this release deletes. A login .zlogin
	// that `exec`s away now takes care of itself - it never reaches a prompt, so
	// it never announces, so the bit stays "0".
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
		shimFuncDef(filepath.Join(sessionDir, ShimStatusFilename)) + "\n" +
		promptHookBody + "\n" +
		shimRegisterLine + "\n" +
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

// writeShimStatus records broker shim-readiness to the session dir so the
// spawning ptyclient can read it after dial. Mirrors writeHookStatus.
func writeShimStatus(sessionDir string, ready bool, log *slog.Logger) {
	val := []byte("0")
	if ready {
		val = []byte("1")
	}
	path := filepath.Join(sessionDir, ShimStatusFilename)
	if err := os.WriteFile(path, val, 0o600); err != nil {
		log.Warn("sidecar.hookseed.shim_status_write_failed", "err", err.Error())
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
