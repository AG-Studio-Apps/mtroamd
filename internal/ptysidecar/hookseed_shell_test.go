package ptysidecar

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests EXECUTE a real shell through the real seed functions and assert
// on the shell's LIVE environment, rather than on the text of the generated
// rcfile. That distinction is the whole point: the shim-ordering defect this
// suite pins was invisible to a string-matching test, because the generated
// line was present and correct-looking while the resulting PATH was wrong.
//
// They are also the macOS coverage. On a Mac the daemon runs as a LaunchAgent
// with launchd's default PATH and the session shell is a NON-login zsh, which
// never runs /usr/libexec/path_helper (it is invoked from /etc/zprofile, a
// login file). Running this suite on a macOS runner is what turns that from a
// prediction into a fact.

// lookShell resolves a shell or skips the test. Shells are environment, not
// dependencies: a box without zsh should skip, never fail.
func lookShell(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed on this box", name)
	}
	return p
}

// Probes wrap their answer in markers because the shell's own startup files
// write to STDOUT. Ubuntu's /etc/bash.bashrc prints the "run a command as
// administrator" notice, and any real user's rc may echo a banner; reading raw
// stdout captured that chatter as if it were the value. Distro chatter is the
// normal case, not an edge case, so the harness has to tolerate it.
const (
	probeBegin = "__MT_BEGIN__"
	probeEnd   = "__MT_END__"
)

// probeFor wraps a shell expression so its value can be recovered from noisy
// stdout.
func probeFor(expr string) string {
	return `printf '` + probeBegin + `%s` + probeEnd + `' "` + expr + `"`
}

// extractProbe pulls the marked value out of noisy stdout.
func extractProbe(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, probeBegin)
	j := strings.Index(out, probeEnd)
	if i < 0 || j < i {
		t.Fatalf("probe markers not found in output: %q", out)
	}
	return out[i+len(probeBegin) : j]
}

// runSeeded seeds a shell for a throwaway session + HOME, then runs `probe`
// inside it and returns stdout.
//
// `-i` is appended because bash reads --rcfile, and zsh reads .zshrc, ONLY for
// interactive shells. The sidecar gets that for free by handing the shell a
// PTY; a test without a TTY has to ask for it explicitly. bash warns about job
// control on stderr in that case, which is why only stdout is returned.
func runSeeded(t *testing.T, shellPath, home string, args []string, probe string) (value, shimDir string) {
	t.Helper()
	sessionDir := t.TempDir()
	shimDir = filepath.Join(sessionDir, "shims")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatalf("mkdir shims: %v", err)
	}

	// Mirror the sidecar's spawn env: the shim dir is FIRST at spawn, and the
	// user's rc then gets a chance to push it back.
	env := []string{
		"HOME=" + home,
		"PATH=" + shimDir + ":/usr/bin:/bin",
		"MESHTERM_SHIM_DIR=" + shimDir,
		"MESHTERM_SESSION_ID=testsession",
		"TERM=xterm",
	}

	res := seedPromptHook(sessionDir, shellPath, args, env, "testnonce", discardLogger())
	if !res.hookInstalled {
		t.Fatalf("seedPromptHook did not install a hook for %s", shellPath)
	}

	cmd := exec.Command(res.shell, append(append([]string{}, res.args...), "-i", "-c", probe)...)
	cmd.Env = res.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running seeded %s: %v (out=%q)", shellPath, err, string(out))
	}
	return extractProbe(t, string(out)), shimDir
}

// writeHome builds a throwaway HOME whose rc file does the single most ordinary
// thing a real dotfile does, and the thing that broke the shim: prepend a
// personal bin dir to PATH.
func writeHome(t *testing.T, rcName string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "mybin"), 0o700); err != nil {
		t.Fatalf("mkdir mybin: %v", err)
	}
	rc := "PATH=\"$HOME/mybin:$PATH\"\nexport PATH\n"
	if err := os.WriteFile(filepath.Join(home, rcName), []byte(rc), 0o600); err != nil {
		t.Fatalf("write %s: %v", rcName, err)
	}
	return home
}

// TestSeededShellKeepsShimFirst is the regression test for the ordering defect:
// a user rc that prepends to PATH must not be able to outrank the broker shim
// dir, because an outranked shim means `mtroamd secret-exec` never runs and the
// tool starts with no secrets, silently, while shimReady still reports 1.
func TestSeededShellKeepsShimFirst(t *testing.T) {
	cases := []struct {
		shell  string
		rcName string
	}{
		{"bash", ".bashrc"},
		{"zsh", ".zshrc"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			shellPath := lookShell(t, tc.shell)
			home := writeHome(t, tc.rcName)
			got, shimDir := runSeeded(t, shellPath, home, nil, probeFor("$PATH"))

			entries := strings.Split(got, ":")
			if len(entries) == 0 || entries[0] == "" {
				t.Fatalf("empty PATH from seeded %s: %q", tc.shell, got)
			}
			if entries[0] != shimDir {
				t.Errorf("shim dir is not FIRST on PATH\n first = %q\n want  = %q\n PATH  = %s",
					entries[0], shimDir, got)
			}
			// The user's own prepend must survive: we reorder, never discard.
			if !strings.Contains(got, filepath.Join(home, "mybin")) {
				t.Errorf("user's rc PATH prepend was lost: %s", got)
			}
			// And exactly once, so the line can never grow PATH.
			if n := strings.Count(":"+got+":", ":"+shimDir+":"); n != 1 {
				t.Errorf("shim dir appears %d times, want 1: %s", n, got)
			}
		})
	}
}

// TestSeededShellShimActuallyWins is the hazard test, not the mechanism test.
// PATH order is a proxy; what matters is which binary the shell RESOLVES. This
// puts a decoy of the same name in the user's own bin dir, which is exactly
// what a brew/gnubin or ~/.local/bin install does.
func TestSeededShellShimActuallyWins(t *testing.T) {
	cases := []struct {
		shell  string
		rcName string
	}{
		{"bash", ".bashrc"},
		{"zsh", ".zshrc"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			shellPath := lookShell(t, tc.shell)
			home := writeHome(t, tc.rcName)

			decoy := filepath.Join(home, "mybin", "printenv")
			if err := os.WriteFile(decoy, []byte("#!/bin/sh\necho DECOY\n"), 0o700); err != nil {
				t.Fatalf("write decoy: %v", err)
			}

			sessionDir := t.TempDir()
			shimDir := filepath.Join(sessionDir, "shims")
			if err := os.MkdirAll(shimDir, 0o700); err != nil {
				t.Fatalf("mkdir shims: %v", err)
			}
			shim := filepath.Join(shimDir, "printenv")
			if err := os.WriteFile(shim, []byte("#!/bin/sh\necho SHIM\n"), 0o700); err != nil {
				t.Fatalf("write shim: %v", err)
			}

			env := []string{
				"HOME=" + home,
				"PATH=" + shimDir + ":/usr/bin:/bin",
				"MESHTERM_SHIM_DIR=" + shimDir,
				"MESHTERM_SESSION_ID=testsession",
				"TERM=xterm",
			}
			res := seedPromptHook(sessionDir, shellPath, nil, env, "testnonce", discardLogger())
			// `$(printenv)` so the marker wrapper captures which binary ran,
			// isolated from the rc's own stdout chatter.
			probe := probeFor("$(printenv)")
			cmd := exec.Command(res.shell, append(append([]string{}, res.args...), "-i", "-c", probe)...)
			cmd.Env = res.env
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("running seeded %s: %v (out=%q)", tc.shell, err, string(out))
			}
			if got := strings.TrimSpace(extractProbe(t, string(out))); got != "SHIM" {
				t.Errorf("user's binary shadowed the broker shim: ran %q, want \"SHIM\"", got)
			}
		})
	}
}

// TestSeededShellInstallsHook guards the other half of the rcfile: the
// live-inject hook has to survive the user's rc, which is why it is appended
// after it.
func TestSeededShellInstallsHook(t *testing.T) {
	cases := []struct {
		shell  string
		rcName string
		probe  string
	}{
		// A user PROMPT_COMMAND / precmd must not displace ours.
		{"bash", ".bashrc", probeFor("$PROMPT_COMMAND")},
		{"zsh", ".zshrc", probeFor("${precmd_functions[*]}")},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			shellPath := lookShell(t, tc.shell)
			home := t.TempDir()
			rc := "PROMPT_COMMAND=user_thing\nprecmd_functions+=(user_thing)\nuser_thing(){ :; }\n"
			if err := os.WriteFile(filepath.Join(home, tc.rcName), []byte(rc), 0o600); err != nil {
				t.Fatalf("write rc: %v", err)
			}
			got, _ := runSeeded(t, shellPath, home, nil, tc.probe)
			if !strings.Contains(got, "_mt_inject") {
				t.Errorf("_mt_inject not registered for %s: %q", tc.shell, got)
			}
			if !strings.Contains(got, "user_thing") {
				t.Errorf("user's own prompt hook was clobbered for %s: %q", tc.shell, got)
			}
		})
	}
}

// TestSeededShellSurvivesAPerPromptPathMutator is the headline regression.
//
// ★★ A one-shot re-assert at rc time is NOT enough. direnv, mise, asdf, conda
// and nvm all install a PROMPT_COMMAND / precmd hook that re-prepends their own
// dir on EVERY prompt, which lands AFTER our rc line has run. PATH is therefore
// correct at the instant the rcfile finishes and wrong from prompt one onward,
// so a test that only inspects the post-rc PATH (as the first version of this
// suite did) passes while the shim loses every time it matters.
//
// The probe runs the registered prompt hook explicitly rather than trying to
// drive a real prompt, because a PTY-less test shell never renders one.
func TestSeededShellSurvivesAPerPromptPathMutator(t *testing.T) {
	cases := []struct {
		shell  string
		rcName string
		// Re-runs whatever the shell registered as its prompt hook, the way a
		// real prompt would.
		fire string
	}{
		{"bash", ".bashrc", `eval "$PROMPT_COMMAND"`},
		{"zsh", ".zshrc", `for f in $precmd_functions; do $f; done`},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			shellPath := lookShell(t, tc.shell)
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, "mybin"), 0o700); err != nil {
				t.Fatalf("mkdir mybin: %v", err)
			}
			// A direnv-shaped hook: prepend our dir on every prompt.
			rc := "_user_hook(){ PATH=\"$HOME/mybin:$PATH\"; }\n" +
				"if [ -n \"$ZSH_VERSION\" ]; then precmd_functions+=(_user_hook); " +
				"else PROMPT_COMMAND=\"${PROMPT_COMMAND:+$PROMPT_COMMAND; }_user_hook\"; fi\n"
			if err := os.WriteFile(filepath.Join(home, tc.rcName), []byte(rc), 0o600); err != nil {
				t.Fatalf("write rc: %v", err)
			}

			// Fire the prompt hook three times, then report PATH.
			probe := tc.fire + "; " + tc.fire + "; " + tc.fire + "; " + probeFor("$PATH")
			got, shimDir := runSeeded(t, shellPath, home, nil, probe)

			entries := strings.Split(got, ":")
			if len(entries) == 0 || entries[0] != shimDir {
				t.Errorf("shim lost its place to a per-prompt PATH mutator\n first = %q\n want  = %q\n PATH  = %s",
					entries[0], shimDir, got)
			}
			if n := strings.Count(":"+got+":", ":"+shimDir+":"); n != 1 {
				t.Errorf("shim dir appears %d times after 3 prompts, want 1: %s", n, got)
			}
			if !strings.Contains(got, filepath.Join(home, "mybin")) {
				t.Errorf("the user's own dir was lost: %s", got)
			}
		})
	}
}

// announceStatus seeds a shell for a throwaway session whose HOME carries the
// given rc, runs it, and returns what ended up in the shim-ready file.
//
// It pre-writes "0" exactly as the sidecar does, so the test sees the real
// production starting point: not-ready until the shell earns it. The return is
// the raw file contents ("1", "0") or "<absent>".
//
// Errors from the shell run are deliberately ignored. One of the cases under
// test is an rc that `exec`s away, where the exit status belongs to whatever it
// exec'd, and the only thing that matters is what the file says afterwards.
func announceStatus(t *testing.T, shellPath, home string, args []string) string {
	t.Helper()
	sessionDir := t.TempDir()
	shimDir := filepath.Join(sessionDir, "shims")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatalf("mkdir shims: %v", err)
	}
	statusPath := filepath.Join(sessionDir, ShimStatusFilename)
	if err := os.WriteFile(statusPath, []byte("0"), 0o600); err != nil {
		t.Fatalf("seed status file: %v", err)
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + shimDir + ":/usr/bin:/bin",
		"MESHTERM_SHIM_DIR=" + shimDir,
		"MESHTERM_SESSION_ID=testsession",
		"TERM=xterm",
	}
	res := seedPromptHook(sessionDir, shellPath, args, env, "testnonce", discardLogger())
	if !res.hookInstalled {
		t.Fatalf("seedPromptHook did not install a hook for %s", shellPath)
	}
	// ★★ The announce now fires only from the PER-PROMPT hook, so the shell has
	// to actually reach a prompt. `-i -c cmd` never displays one. Feeding a
	// command on STDIN of an interactive shell does: bash/zsh run the prompt
	// cycle (and therefore PROMPT_COMMAND / precmd_functions) per input line.
	cmd := exec.Command(res.shell, append(append([]string{}, res.args...), "-i")...)
	cmd.Env = res.env
	cmd.Stdin = strings.NewReader("true\nexit\n")
	_ = cmd.Run()
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return "<absent>"
	}
	got := strings.TrimSpace(string(data))
	// Normalise to "1"/"0" so the cases below read as readiness rather than as
	// token comparison. Ready means the shell echoed back THIS spawn's nonce.
	if res.shimNonce != "" && got == res.shimNonce {
		return "1"
	}
	if got == "" {
		return "<empty>"
	}
	return got
}

// writeHomeRC builds a throwaway HOME with an arbitrary rc body.
func writeHomeRC(t *testing.T, rcName, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "mybin"), 0o700); err != nil {
		t.Fatalf("mkdir mybin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, rcName), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", rcName, err)
	}
	return home
}

// TestAnnounceOnlyWhenShellActuallyGetsThere is the regression test for the
// defect that blocked v1.7.11-rc2: readiness was PREDICTED at seed time from
// `!login`, so a shell that never reached its prompt still reported ready. iOS
// then marked a brokered secret `.delivered` while the tool launched with none.
//
// Readiness is now announced by the shell itself. These cases pin the asymmetry
// that makes that safe: a shell that gets through its rc with the shim first
// says so, and a shell that does not says nothing, leaving the sidecar's "0".
func TestAnnounceOnlyWhenShellActuallyGetsThere(t *testing.T) {
	bash := lookShell(t, "bash")

	t.Run("ordinary_rc_announces", func(t *testing.T) {
		// The everyday case: an rc that prepends a personal bin dir. The shim is
		// re-asserted to the front, so the shell is genuinely ready and says so.
		home := writeHomeRC(t, ".bashrc", "PATH=\"$HOME/mybin:$PATH\"\nexport PATH\n")
		if got := announceStatus(t, bash, home, nil); got != "1" {
			t.Errorf("ordinary rc: shim-ready = %q, want \"1\"", got)
		}
	})

	t.Run("rc_that_execs_away_never_announces", func(t *testing.T) {
		// ★★ THE headline case. `[ -z "$TMUX" ] && exec tmux new-session -A -s main`
		// is a near-universal dotfile idiom, and the generated rcfile sources the
		// user's rc BEFORE our shim lines, so the exec means they never run.
		// rc2 reported this session READY because the shell merely was not a login
		// shell. It must now stay not-ready.
		home := writeHomeRC(t, ".bashrc", "exec /bin/true\n")
		if got := announceStatus(t, bash, home, nil); got == "1" {
			t.Errorf("rc that execs away: shim-ready = %q, want NOT \"1\" (the shell never reached our lines)", got)
		}
	})

	t.Run("set_u_rc_still_announces_and_stays_quiet", func(t *testing.T) {
		// `set -u` turned the old bare `$ZSH_VERSION` into an unbound-variable
		// error that aborted the register line, so _mt_shim_path was never wired
		// up - while the seed-time bit still said ready. With `${ZSH_VERSION:-}`
		// the line survives and the shell announces honestly.
		home := writeHomeRC(t, ".bashrc", "set -u\nPATH=\"$HOME/mybin:$PATH\"\nexport PATH\n")
		if got := announceStatus(t, bash, home, nil); got != "1" {
			t.Errorf("set -u rc: shim-ready = %q, want \"1\"", got)
		}
	})

	t.Run("login_shell_that_reaches_a_prompt_is_ready", func(t *testing.T) {
		// The flip side of dropping the `!login` prediction: a login shell whose
		// profile does NOT exec away is perfectly capable of being ready, and
		// rc1/rc2 reported it not-ready purely because it was a login shell. That
		// was a false NEGATIVE - a permanent, unactionable "regenerate" warning.
		home := writeHomeRC(t, ".bash_profile", "PATH=\"$HOME/mybin:$PATH\"\nexport PATH\n")
		if got := announceStatus(t, bash, home, []string{"-l"}); got != "1" {
			t.Errorf("login bash reaching a prompt: shim-ready = %q, want \"1\"", got)
		}
	})
}

// TestLoginZshThatExecsAwayNeverAnnounces is the regression test for the defect
// that blocked v1.7.11-rc3, and it is worth its own function because rc3 made
// this case WORSE than the rc2 it replaced.
//
// rc2 predicted readiness as `!login` and so reported this session not-ready,
// which was right by accident. rc3 replaced the prediction with an observation
// but took the observation at the wrong moment - the end of the rc rather than a
// prompt - so the shell announced ready and then exec'd into a tmux whose panes
// had never been seeded. Measured on zsh 5.9 during review.
//
// zsh reads .zshrc and THEN .zlogin, and our generated .zshrc restores the real
// ZDOTDIR as its last statement, so the user's .zlogin is the real one. An
// `exec` there means no prompt is ever reached, which must mean no announce.
func TestLoginZshThatExecsAwayNeverAnnounces(t *testing.T) {
	zsh := lookShell(t, "zsh")

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "mybin"), 0o700); err != nil {
		t.Fatalf("mkdir mybin: %v", err)
	}
	// An ordinary rc: prepend a personal bin dir. On its own this session would
	// be perfectly ready.
	if err := os.WriteFile(filepath.Join(home, ".zshrc"),
		[]byte("PATH=\"$HOME/mybin:$PATH\"\nexport PATH\n"), 0o600); err != nil {
		t.Fatalf("write .zshrc: %v", err)
	}
	// ...but the login file execs away before any prompt. This is the common
	// `[[ -z $TMUX ]] && exec tmux` shape, reduced to something that terminates.
	if err := os.WriteFile(filepath.Join(home, ".zlogin"),
		[]byte("exec /bin/true\n"), 0o600); err != nil {
		t.Fatalf("write .zlogin: %v", err)
	}

	if got := announceStatus(t, zsh, home, []string{"-l"}); got == "1" {
		t.Errorf("login zsh whose .zlogin execs away: shim-ready = %q, want NOT \"1\" "+
			"(it never reached a prompt, so it cannot have observed anything)", got)
	}
}

// TestRehashActuallyRedirectsAToolThatWasRunFirst is the test whose ABSENCE let
// the same defect ship three times.
//
// The stale-command-hash bug was filed against rc2, "fixed" in rc3 by putting
// `hash -r` on a branch the default spawn never takes, and "fixed" again in rc4
// with a generation mechanism that was entirely inert because `read` returns 1
// at EOF and the `|| _mt_g=` arm wiped the value it had just read. Both times
// the code looked right and no test executed it.
//
// This drives the real production sequence in a real shell: the shim dir is
// first on PATH from spawn but EMPTY, the user runs a tool (so the shell hashes
// the real binary), the shim appears later exactly as SyncShims creates it, the
// daemon bumps the generation, and the next prompt must make the tool resolve to
// the shim.
func TestRehashActuallyRedirectsAToolThatWasRunFirst(t *testing.T) {
	bash := lookShell(t, "bash")

	sessionDir := t.TempDir()
	shimDir := filepath.Join(sessionDir, "shims")
	realDir := filepath.Join(sessionDir, "realbin")
	for _, d := range []string{shimDir, realDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// The real binary the shell will hash on first use.
	if err := os.WriteFile(filepath.Join(realDir, "mytool"),
		[]byte("#!/bin/sh\necho REAL-BINARY\n"), 0o700); err != nil {
		t.Fatalf("write real mytool: %v", err)
	}
	genPath := filepath.Join(sessionDir, ShimGenFilename)
	if err := os.WriteFile(genPath, []byte("gen1\n"), 0o600); err != nil {
		t.Fatalf("write gen: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, ShimStatusFilename), []byte("0"), 0o600); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(""), 0o600); err != nil {
		t.Fatalf("write .bashrc: %v", err)
	}

	env := []string{
		"HOME=" + home,
		"PATH=" + shimDir + ":" + realDir + ":/usr/bin:/bin",
		"MESHTERM_SHIM_DIR=" + shimDir,
		"MESHTERM_SESSION_ID=testsession",
		"TERM=xterm",
	}
	res := seedPromptHook(sessionDir, bash, nil, env, "leasenonce", discardLogger())
	if !res.hookInstalled {
		t.Fatal("bash was not seeded")
	}

	// ★★ The assertion is on the MECHANISM, not on a downstream side effect.
	//
	// An earlier version of this test checked only that the tool resolved to the
	// shim afterwards - and it PASSED with the rc4 bug reintroduced, because bash
	// clears its hash table for other reasons too. A test that cannot fail on the
	// broken code is worse than no test: it is what let this defect ship twice.
	//
	// ★ The generation is written WITHOUT a trailing newline, deliberately. That
	// is the hostile shape - it is exactly what rc4's bumpShimGen produced - and
	// it is the only shape that distinguishes the two implementations, because
	// `read` returns 0 (not 1) when the file DOES end in a newline, which would
	// mask the bug entirely. The daemon now writes a newline as well, but the
	// hook must not depend on it.
	//
	// `_MT_SHIM_GEN` is unambiguous. With the fix, `read` populates `_mt_g`, it
	// differs from the empty `_MT_SHIM_GEN`, and the variable is assigned. With
	// the `|| _mt_g=` bug, `_mt_g` is wiped, compares equal to the equally-empty
	// `_MT_SHIM_GEN`, and the function returns before assigning anything - so the
	// variable stays unset and nothing is ever rehashed.
	script := "mytool\n" +
		"printf '#!/bin/sh\\necho SHIM-BINARY\\n' > " + shSingleQuote(filepath.Join(shimDir, "mytool")) + "\n" +
		"chmod 0700 " + shSingleQuote(filepath.Join(shimDir, "mytool")) + "\n" +
		"printf 'gen2' > " + shSingleQuote(genPath) + "\n" +
		"true\n" +
		"echo \"MTGEN=[${_MT_SHIM_GEN-unset}]\"\n" +
		"mytool\n" +
		"exit\n"

	cmd := exec.Command(res.shell, append(append([]string{}, res.args...), "-i")...)
	cmd.Env = res.env
	cmd.Stdin = strings.NewReader(script)
	out, _ := cmd.Output()
	got := string(out)

	if !strings.Contains(got, "REAL-BINARY") {
		t.Fatalf("setup failed: the first call never resolved the real binary.\nout=%q", got)
	}
	// The generation the daemon wrote must have been observed and recorded.
	if !strings.Contains(got, "MTGEN=[gen2]") {
		t.Errorf("_mt_shim_rehash never picked up the bumped generation "+
			"(want MTGEN=[gen2]); the rehash is inert, which is the rc3+rc4 defect.\nout=%q", got)
	}
	if strings.Contains(got, "MTGEN=[unset]") {
		t.Errorf("_MT_SHIM_GEN was never assigned - `read` populated nothing, so "+
			"hash -r can never run.\nout=%q", got)
	}
	if !strings.Contains(got, "SHIM-BINARY") {
		t.Errorf("after the shim appeared and the generation was bumped, the tool still "+
			"resolved to the real binary.\nout=%q", got)
	}
}

// TestLeaseRenewsOnEveryPrompt pins the property that makes the lease a lease.
// rc3 and rc4 both wrote the claim exactly once and latched it, which is why an
// `exec`ed-away shell stayed "ready" forever.
//
// ★★ Both prompts happen in the SAME shell, and the lease is aged from INSIDE
// the script. An earlier version used two separate shell invocations and passed
// against a latched implementation, because a per-shell latch grants one write
// per shell - so two runs produced two writes and looked identical to a lease.
// The distinguishing question is whether a SECOND prompt in an ALREADY-ANNOUNCED
// shell rewrites the file, and only a single-session test can ask it.
func TestLeaseRenewsOnEveryPrompt(t *testing.T) {
	bash := lookShell(t, "bash")

	sessionDir := t.TempDir()
	shimDir := filepath.Join(sessionDir, "shims")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatalf("mkdir shims: %v", err)
	}
	statusPath := filepath.Join(sessionDir, ShimStatusFilename)
	if err := os.WriteFile(statusPath, []byte("0"), 0o600); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(""), 0o600); err != nil {
		t.Fatalf("write .bashrc: %v", err)
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + shimDir + ":/usr/bin:/bin",
		"MESHTERM_SHIM_DIR=" + shimDir,
		"MESHTERM_SESSION_ID=testsession",
		"TERM=xterm",
	}
	res := seedPromptHook(sessionDir, bash, nil, env, "leasenonce", discardLogger())
	if !res.hookInstalled {
		t.Fatal("bash was not seeded")
	}

	// Prompt 1 renews the lease. Then age it to 2001 from inside the session and
	// take prompt 2, which must renew it again.
	q := shSingleQuote(statusPath)
	// ★ The age-and-observe must be ONE line: a prompt fires between every input
	// line and renews the lease, so a `touch` on its own line is undone before the
	// next line can read it. AFTER is then read on the FOLLOWING line, i.e. after
	// exactly one intervening prompt - which is the thing under test.
	script := "true\n" +
		"touch -t 200101010000 " + q + "; echo \"AGED=$(date -r " + q + " +%Y)\"\n" +
		"echo \"AFTER=$(date -r " + q + " +%Y)\"\n" +
		"exit\n"

	cmd := exec.Command(res.shell, append(append([]string{}, res.args...), "-i")...)
	cmd.Env = res.env
	cmd.Stdin = strings.NewReader(script)
	out, _ := cmd.Output()
	got := string(out)

	if !strings.Contains(got, "AGED=2001") {
		t.Fatalf("setup failed: the lease was not aged.\nout=%q", got)
	}
	thisYear := time.Now().Format("2006")
	if !strings.Contains(got, "AFTER="+thisYear) {
		t.Errorf("after a LATER prompt in the same shell the lease mtime is still %s, "+
			"want %s. The claim is latched (write-once), not leased - which is exactly "+
			"why rc3/rc4 kept reporting ready after an `exec`.\nout=%q",
			strings.TrimSpace(got[strings.Index(got, "AFTER="):]), thisYear, got)
	}

	data, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if strings.TrimSpace(string(data)) != res.shimNonce {
		t.Errorf("lease content = %q, want the spawn nonce %q", string(data), res.shimNonce)
	}
}
