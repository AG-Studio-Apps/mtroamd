package secret

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePayloadValidatesKeysAndCommands(t *testing.T) {
	good := `{"secrets":[{"key":"GITHUB_TOKEN","value":"ghp_x","cmds":["gh","git"]}]}`
	p, err := ParsePayload([]byte(good))
	if err != nil {
		t.Fatalf("good payload: %v", err)
	}
	if len(p.Secrets) != 1 || p.Secrets[0].Key != "GITHUB_TOKEN" ||
		!reflect.DeepEqual(p.Secrets[0].Cmds, []string{"gh", "git"}) {
		t.Fatalf("round-trip mismatch: %+v", p)
	}

	for _, bad := range []string{
		`{"secrets":[{"key":"9BAD","value":"x","cmds":["gh"]}]}`,     // key not POSIX
		`{"secrets":[{"key":"OK","value":"x","cmds":["../evil"]}]}`,  // command escapes basename
		`{"secrets":[{"key":"OK","value":"x","cmds":["a b"]}]}`,      // whitespace in command
		`{"secrets":[{"key":"OK","value":"x","cmds":["gh;rm"]}]}`,    // shell metachar
		`{"secrets":[{"key":"A","value":"1","cmds":[]},{"key":"A","value":"2","cmds":[]}]}`, // dup key
		`{"secrets":[{"key":"A","value":"1","cmds":[],"nope":true}]}`, // unknown field
	} {
		if _, err := ParsePayload([]byte(bad)); err == nil {
			t.Errorf("expected rejection for %s", bad)
		}
	}
}

func TestStoreEnvForCommandIsLeastPrivilege(t *testing.T) {
	s := NewStore()
	s.SetSession("sid1", Payload{Secrets: []Entry{
		{Key: "GITHUB_TOKEN", Value: "ghp", Cmds: []string{"gh", "git"}},
		{Key: "AWS_KEY", Value: "akia", Cmds: []string{"aws"}},
	}})

	// gh gets only GITHUB_TOKEN, never AWS_KEY.
	gh := s.EnvForCommand("sid1", "gh")
	if !reflect.DeepEqual(gh, map[string]string{"GITHUB_TOKEN": "ghp"}) {
		t.Errorf("gh env = %v, want only GITHUB_TOKEN", gh)
	}
	// aws gets only AWS_KEY.
	if aws := s.EnvForCommand("sid1", "aws"); !reflect.DeepEqual(aws, map[string]string{"AWS_KEY": "akia"}) {
		t.Errorf("aws env = %v", aws)
	}
	// An undeclared command gets nothing (the value never reaches it).
	if curl := s.EnvForCommand("sid1", "curl"); curl != nil {
		t.Errorf("curl env = %v, want nil (undeclared)", curl)
	}
	// Unknown session gets nothing.
	if x := s.EnvForCommand("nope", "gh"); x != nil {
		t.Errorf("unknown session env = %v", x)
	}
	if got, want := s.Commands("sid1"), []string{"aws", "gh", "git"}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}
}

func TestStoreSetReplacesAndEmptyClears(t *testing.T) {
	s := NewStore()
	s.SetSession("sid", Payload{Secrets: []Entry{{Key: "A", Value: "1", Cmds: []string{"gh"}}}})
	// A full push REPLACES: rotating to a set without A stops delivering A.
	s.SetSession("sid", Payload{Secrets: []Entry{{Key: "B", Value: "2", Cmds: []string{"gh"}}}})
	env := s.EnvForCommand("sid", "gh")
	if _, hasA := env["A"]; hasA {
		t.Error("A still present after replace")
	}
	if env["B"] != "2" {
		t.Errorf("B = %q, want 2", env["B"])
	}
	// Empty push clears.
	s.SetSession("sid", Payload{})
	if env := s.EnvForCommand("sid", "gh"); env != nil {
		t.Errorf("env after empty push = %v, want nil", env)
	}
}

func TestSyncShimsWritesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	shimDir := filepath.Join(dir, "shims")
	if err := SyncShims(shimDir, "/opt/mtroamd", []string{"gh", "git"}); err != nil {
		t.Fatal(err)
	}
	// Both shims exist, executable, and re-exec into secret-exec.
	for _, c := range []string{"gh", "git"} {
		b, err := os.ReadFile(filepath.Join(shimDir, c))
		if err != nil {
			t.Fatalf("read shim %s: %v", c, err)
		}
		if want := "exec '/opt/mtroamd' secret-exec " + c + " -- \"$@\""; !contains(string(b), want) {
			t.Errorf("shim %s body = %q, missing %q", c, b, want)
		}
		info, _ := os.Stat(filepath.Join(shimDir, c))
		if info.Mode()&0o100 == 0 {
			t.Errorf("shim %s not executable", c)
		}
	}
	// A narrowed set prunes the dropped shim (git removed).
	if err := SyncShims(shimDir, "/opt/mtroamd", []string{"gh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shimDir, "git")); !os.IsNotExist(err) {
		t.Error("git shim should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(shimDir, "gh")); err != nil {
		t.Error("gh shim should remain")
	}
}

func TestResolveRealSkipsShimDir(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "bin")
	for _, d := range []string{shimDir, realDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A shim named `gh` in shimDir, and the real `gh` in realDir.
	mustExec(t, filepath.Join(shimDir, "gh"))
	mustExec(t, filepath.Join(realDir, "gh"))
	// PATH lists shimDir FIRST, but ResolveReal must skip it and find realDir's.
	pathEnv := shimDir + string(os.PathListSeparator) + realDir
	got, err := ResolveReal("gh", shimDir, pathEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(realDir, "gh") {
		t.Errorf("resolved %q, want the real (non-shim) gh", got)
	}
	// A command that only exists as a shim resolves to nothing (no infinite loop).
	mustExec(t, filepath.Join(shimDir, "solo"))
	if _, err := ResolveReal("solo", shimDir, pathEnv); err == nil {
		t.Error("expected not-found when only the shim exists")
	}
}

func mustExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
