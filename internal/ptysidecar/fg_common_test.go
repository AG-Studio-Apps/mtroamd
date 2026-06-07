package ptysidecar

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// makeProcArgs builds a synthetic KERN_PROCARGS2 buffer:
// [int32 argc][exec_path \0][\0 pad][argv0 \0]…[env…].
func makeProcArgs(argc int32, execPath string, pad int, argv []string, env []string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, argc)
	b.WriteString(execPath)
	b.WriteByte(0)
	for i := 0; i < pad; i++ {
		b.WriteByte(0)
	}
	for _, a := range argv {
		b.WriteString(a)
		b.WriteByte(0)
	}
	for _, e := range env {
		b.WriteString(e)
		b.WriteByte(0)
	}
	return b.Bytes()
}

func TestParseProcArgs(t *testing.T) {
	t.Parallel()
	// Normal: node wrapper + script + trailing env (env must be ignored).
	buf := makeProcArgs(2, "/usr/local/bin/node", 3,
		[]string{"node", "/usr/local/bin/claude"},
		[]string{"PATH=/usr/bin", "HOME=/Users/x"})
	got := parseProcArgs(buf)
	if len(got) != 2 {
		t.Fatalf("argc=2: got %d args %q", len(got), got)
	}
	if string(got[0]) != "node" || string(got[1]) != "/usr/local/bin/claude" {
		t.Fatalf("argv mismatch: %q", got)
	}

	// Single arg, no padding.
	got = parseProcArgs(makeProcArgs(1, "/bin/vim", 0, []string{"vim"}, nil))
	if len(got) != 1 || string(got[0]) != "vim" {
		t.Fatalf("single arg: %q", got)
	}

	// Truncated mid-argv: declared argc 3 but only 1 arg present, last
	// arg unterminated → best-effort, no panic, no over-read.
	trunc := makeProcArgs(3, "/bin/sh", 1, nil, nil)
	trunc = append(trunc, []byte("ba")...) // "ba" with no trailing NUL
	got = parseProcArgs(trunc)
	if len(got) != 1 || string(got[0]) != "ba" {
		t.Fatalf("truncated: %q", got)
	}

	// Malformed / hostile inputs must degrade to nil, never panic.
	for name, bad := range map[string][]byte{
		"empty":       {},
		"short":       {1, 2, 3},
		"argc<=0":     makeProcArgs(0, "/bin/sh", 0, []string{"sh"}, nil),
		"argc-huge":   makeProcArgs(1<<20, "/bin/sh", 0, []string{"sh"}, nil),
		"no-exec-nul": binary.LittleEndian.AppendUint32(nil, 2), // argc only, no exec_path NUL
	} {
		if got := parseProcArgs(bad); got != nil {
			t.Errorf("%s: expected nil, got %q", name, got)
		}
	}
}

// FuzzParseProcArgs asserts the KERN_PROCARGS2 decoder never panics on
// arbitrary input — it decodes an attacker-influenceable kernel buffer,
// and gosec/govulncheck can't reason about slice bounds. Seed with a
// well-formed buffer; the fuzzer mutates toward truncation/garbage.
// (Re-run on any change to parseProcArgs — the table test is necessary
// but not sufficient.)
func FuzzParseProcArgs(f *testing.F) {
	f.Add(makeProcArgs(2, "/usr/local/bin/node", 3,
		[]string{"node", "/usr/local/bin/claude"}, []string{"PATH=/usr/bin"}))
	f.Add([]byte{})
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(_ *testing.T, buf []byte) {
		_ = parseProcArgs(buf) // must not panic / OOB / hang
	})
}

func TestCommFromArgs(t *testing.T) {
	t.Parallel()
	bb := func(ss ...string) [][]byte {
		out := make([][]byte, len(ss))
		for i, s := range ss {
			out[i] = []byte(s)
		}
		return out
	}
	cases := []struct {
		name string
		args [][]byte
		want string
	}{
		{"plain", bb("/usr/bin/vim"), "vim"},
		{"interpreter+script", bb("node", "/usr/local/bin/claude"), "claude"},
		{"interpreter+flags+script", bb("/usr/bin/python3", "-X", "utf8", "/opt/codex/cli.py"), "cli.py"},
		{"interpreter-only", bb("node"), "node"}, // no script arg → argv0 basename
		{"empty", bb(), ""},
		{"empty-argv0", bb(""), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := commFromArgs(tc.args); got != tc.want {
				t.Errorf("commFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
