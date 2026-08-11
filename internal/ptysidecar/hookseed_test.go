package ptysidecar

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestClassifyShell(t *testing.T) {
	cases := []struct {
		path string
		want shellKind
	}{
		{"/bin/bash", shellBash},
		{"/usr/local/bin/bash", shellBash},
		{"/usr/bin/zsh", shellZsh},
		{"/bin/sh", shellPosix},
		{"/usr/bin/dash", shellPosix},
		{"/usr/bin/fish", shellUnknown},
		{"/bin/cat", shellUnknown},
		{"", shellUnknown},
	}
	for _, tc := range cases {
		if got := classifyShell(tc.path); got != tc.want {
			t.Errorf("classifyShell(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}

func TestClassifyShellArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantBenign bool
		wantLogin  bool
	}{
		{"empty", nil, true, false},
		{"login_long", []string{"--login"}, true, true},
		{"login_short", []string{"-l"}, true, true},
		{"interactive", []string{"-i"}, true, false},
		{"cluster_il", []string{"-il"}, true, true},
		{"cluster_li", []string{"-li"}, true, true},
		{"dash_c_command", []string{"-c", "echo hi"}, false, false},
		{"script_path", []string{"/tmp/run.sh"}, false, false},
		{"unknown_long", []string{"--noprofile"}, false, false},
		{"unknown_short", []string{"-x"}, false, false},
		{"bare_dash", []string{"-"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			benign, login := classifyShellArgs(tc.args)
			if benign != tc.wantBenign || login != tc.wantLogin {
				t.Errorf("classifyShellArgs(%v) = (benign=%v, login=%v), want (benign=%v, login=%v)",
					tc.args, benign, login, tc.wantBenign, tc.wantLogin)
			}
		})
	}
}

// TestSeedPromptHook covers the per-shell seeding decisions: which
// invocation is produced, whether a working hook is reported, and that
// the generated rc chains the user's real files + the byte-exact hook
// body after them.
func TestSeedPromptHook(t *testing.T) {
	const sess = "MESHTERM_SESSION_ID=deadbeef"
	const home = "HOME=/home/tester"

	t.Run("bash_non_login", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/bin/bash", nil, []string{home, sess}, "testnonce", discardLogger())
		if !got.hookInstalled {
			t.Fatalf("hookInstalled = false, want true")
		}
		if got.shell != "/bin/bash" {
			t.Errorf("shell = %q, want /bin/bash", got.shell)
		}
		if len(got.args) != 2 || got.args[0] != "--rcfile" {
			t.Fatalf("args = %v, want [--rcfile <path>]", got.args)
		}
		body := readFile(t, got.args[1])
		mustContain(t, body, `. "$HOME/.bashrc"`)
		mustContain(t, body, promptHookBody)
		// hook must come AFTER the user's rc.
		if strings.Index(body, ".bashrc") > strings.Index(body, promptHookBody) {
			t.Errorf("hook installed before the user's .bashrc")
		}
		// The seeded rc re-asserts the broker shim dir on PATH (after the
		// user's rc) so a login rebuild can't drop it.
		mustContain(t, body, shimRegisterLine)
		// The announce writes to THIS session's status file, by absolute path,
		// and carries this spawn's nonce rather than a bare "1".
		mustContain(t, body, filepath.Join(dir, ShimStatusFilename))
		mustContain(t, body, filepath.Join(dir, ShimGenFilename))
		if got.shimNonce == "" {
			t.Fatal("seeded bash carries no shim nonce")
		}
		mustContain(t, body, got.shimNonce)
	})

	// ★★ This replaces `shim_ready_only_when_seeded`, which asserted the SEED-TIME
	// PREDICTION (`!login`) that this release deletes. There is no longer any
	// readiness value produced at seed time to assert - the shell announces it at
	// its first prompt instead (see hookseed_shell_test.go for the behavioural
	// coverage that actually runs a shell).
	//
	// What is still worth pinning here is the structural half: only a SEEDED
	// shell gets the announce machinery at all, so a shell we never touched can
	// never announce ready no matter what its rc does.
	t.Run("only_seeded_shells_get_the_announce", func(t *testing.T) {
		dir := t.TempDir()

		// Every shape that must NOT be seeded. `-c x` is the tmux shape
		// (`bash -c "tmux new"`): non-benign args, so nothing is seeded and every
		// pane reads the user's real rc.
		for _, tc := range []struct {
			name  string
			shell string
			args  []string
		}{
			{"sh_dash_c", "/bin/sh", []string{"-c", "x"}},
			{"login_sh", "/bin/sh", []string{"-l"}},
			{"fish_unknown", "/usr/bin/fish", nil},
			{"bash_dash_c", "/bin/bash", []string{"-c", "tmux new"}},
		} {
			got := seedPromptHook(dir, tc.shell, tc.args, []string{home, sess}, "testnonce", discardLogger())
			if got.hookInstalled {
				t.Errorf("%s: hookInstalled = true, want false (must not be seeded)", tc.name)
			}
			// Unseeded means the invocation comes back untouched, so there is no
			// generated rc carrying _mt_shim_announce anywhere.
			if got.shell != tc.shell {
				t.Errorf("%s: shell rewritten to %q, want untouched %q", tc.name, got.shell, tc.shell)
			}
		}

		// Seeded shells DO get it, login or not. Login is no longer special: a
		// login startup file that `exec`s away simply never reaches a prompt, so
		// it never announces, and the bit stays "0" on its own.
		statusPath := filepath.Join(dir, ShimStatusFilename)
		_ = statusPath
		for _, tc := range []struct {
			name  string
			shell string
			args  []string
		}{
			{"bash_plain", "/bin/bash", nil},
			{"bash_login", "/bin/bash", []string{"-l"}},
			{"zsh_plain", "/bin/zsh", nil},
			{"zsh_login", "/bin/zsh", []string{"-l"}},
		} {
			got := seedPromptHook(dir, tc.shell, tc.args, []string{home, sess}, "testnonce", discardLogger())
			if !got.hookInstalled {
				t.Fatalf("%s: hookInstalled = false, want true", tc.name)
			}
			body := seededBody(t, dir, got)
			mustContain(t, body, "_mt_shim_renew")
			mustContain(t, body, statusPath)
			// ★ The lease renew must be reachable ONLY through the per-prompt
			// wrapper. rc3 called it from the rc-time line too, which let a
			// login shell claim ready and then `exec` away.
			mustContain(t, body, "_mt_shim_prompt")
			if strings.Contains(body, "fi; _mt_shim_prompt\n") {
				t.Errorf("%s: rc-time immediate call announces; it must call _mt_shim_path only", tc.name)
			}
		}
	})

	t.Run("bash_login", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/bin/bash", []string{"-l"}, []string{home, sess}, "testnonce", discardLogger())
		if !got.hookInstalled {
			t.Fatalf("hookInstalled = false, want true")
		}
		if len(got.args) != 2 || got.args[0] != "--rcfile" {
			t.Fatalf("args = %v, want [--rcfile <path>] (login converted to non-login+profile)", got.args)
		}
		body := readFile(t, got.args[1])
		mustContain(t, body, "/etc/profile")
		mustContain(t, body, `"$HOME/.bash_profile"`)
		mustContain(t, body, promptHookBody)
	})

	t.Run("bash_custom_command_skips", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/bin/bash", []string{"-c", "tmux new"}, []string{home, sess}, "testnonce", discardLogger())
		if got.hookInstalled {
			t.Errorf("hookInstalled = true, want false for a -c command")
		}
		if len(got.args) != 2 || got.args[0] != "-c" {
			t.Errorf("args = %v, want the untouched original", got.args)
		}
	})

	t.Run("zsh_sets_zdotdir", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/usr/bin/zsh", nil, []string{home, sess}, "testnonce", discardLogger())
		if !got.hookInstalled {
			t.Fatalf("hookInstalled = false, want true")
		}
		zdot, ok := envLookup(got.env, "ZDOTDIR")
		if !ok {
			t.Fatalf("ZDOTDIR not set in env")
		}
		// The three startup files must exist and chain the real ones.
		for _, f := range []string{".zshenv", ".zprofile", ".zshrc"} {
			body := readFile(t, filepath.Join(zdot, f))
			if !strings.Contains(body, "$HOME") {
				t.Errorf("%s does not reference the real $HOME zdotdir: %q", f, body)
			}
		}
		zshrc := readFile(t, filepath.Join(zdot, ".zshrc"))
		mustContain(t, zshrc, `. "$HOME"/.zshrc`)
		mustContain(t, zshrc, promptHookBody)
		mustContain(t, zshrc, "unset ZDOTDIR")
		if strings.Index(zshrc, ".zshrc") > strings.Index(zshrc, promptHookBody) {
			t.Errorf("hook installed before the user's .zshrc")
		}
	})

	t.Run("zsh_preserves_existing_zdotdir", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/usr/bin/zsh", nil,
			[]string{home, sess, "ZDOTDIR=/custom/zdot"}, "testnonce", discardLogger())
		zdot, _ := envLookup(got.env, "ZDOTDIR")
		zshrc := readFile(t, filepath.Join(zdot, ".zshrc"))
		mustContain(t, zshrc, "'/custom/zdot'/.zshrc")
		// Restores to the original, not unset.
		mustContain(t, zshrc, "ZDOTDIR='/custom/zdot'")
	})

	t.Run("dash_skipped_untouched", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/bin/dash", nil, []string{home, sess}, "testnonce", discardLogger())
		if got.hookInstalled {
			t.Errorf("hookInstalled = true, want false (dash has no prompt hook)")
		}
		// The shared hook body uses zsh array syntax that dash can't
		// parse, so we must NOT seed a $ENV file — the invocation and
		// env are left untouched.
		if _, ok := envLookup(got.env, "ENV"); ok {
			t.Errorf("ENV was set for dash; want untouched (would spam a parse error)")
		}
		if got.shell != "/bin/dash" || len(got.args) != 0 {
			t.Errorf("invocation mutated: shell=%q args=%v", got.shell, got.args)
		}
	})

	t.Run("unknown_shell_untouched", func(t *testing.T) {
		dir := t.TempDir()
		got := seedPromptHook(dir, "/bin/cat", []string{"-A"}, []string{home, sess}, "testnonce", discardLogger())
		if got.hookInstalled {
			t.Errorf("hookInstalled = true, want false for an unknown shell")
		}
		if got.shell != "/bin/cat" || len(got.args) != 1 || got.args[0] != "-A" {
			t.Errorf("invocation mutated: shell=%q args=%v", got.shell, got.args)
		}
	})
}

func TestWriteHookStatus(t *testing.T) {
	cases := []struct {
		installed bool
		want      string
	}{
		{true, "1"},
		{false, "0"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		writeHookStatus(dir, tc.installed, discardLogger())
		got := readFile(t, filepath.Join(dir, HookStatusFilename))
		if got != tc.want {
			t.Errorf("writeHookStatus(%v) wrote %q, want %q", tc.installed, got, tc.want)
		}
	}
}

func TestEnvReplaceAndLookup(t *testing.T) {
	env := []string{"A=1", "ZDOTDIR=/old", "B=2"}
	out := envReplace(env, "ZDOTDIR", "/new")
	if v, ok := envLookup(out, "ZDOTDIR"); !ok || v != "/new" {
		t.Errorf("ZDOTDIR = %q (ok=%v), want /new", v, ok)
	}
	// Exactly one ZDOTDIR entry survives.
	n := 0
	for _, kv := range out {
		if strings.HasPrefix(kv, "ZDOTDIR=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ZDOTDIR appears %d times, want 1", n)
	}
	// Unrelated keys are preserved.
	if v, ok := envLookup(out, "A"); !ok || v != "1" {
		t.Errorf("A = %q (ok=%v), want 1", v, ok)
	}
}

// seededBody returns the generated startup file a seeded result actually reads:
// bash's --rcfile argument, or the .zshrc in the ZDOTDIR we redirected to. Both
// shells end up sourcing the shim definitions from one of these two places, so a
// test can assert on "what the shell will run" without caring which shell it is.
func seededBody(t *testing.T, dir string, got seedResult) string {
	t.Helper()
	if len(got.args) == 2 && got.args[0] == "--rcfile" {
		return readFile(t, got.args[1])
	}
	return readFile(t, filepath.Join(dir, hookrcSubdir, ".zshrc"))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected to find %q in:\n%s", needle, haystack)
	}
}
