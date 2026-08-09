package ptysidecar

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	res := seedPromptHook(sessionDir, shellPath, args, env, discardLogger())
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
			res := seedPromptHook(sessionDir, shellPath, nil, env, discardLogger())
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
