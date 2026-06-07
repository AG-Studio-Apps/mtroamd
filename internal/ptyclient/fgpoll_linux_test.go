//go:build linux

package ptyclient

import (
	"os"
	"strconv"
	"testing"

	"github.com/AG-Studio-Apps/mtroamd/internal/ptysidecar"
)

// /proc/<pid>/stat field map after the comm field (field 2): the
// returned slice starts at field 3 (state), so ppid (f4)=idx1,
// tpgid (f8)=idx5. The comm field is parenthesized and may contain
// spaces and parens, so parsing must split on the LAST ')'.
func TestParseStatAfterComm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		line      string
		wantPPID  int
		wantTpgid int
	}{
		{
			name: "plain comm",
			// pid (comm) state ppid pgrp sess tty tpgid ...
			line:      "4242 (bash) S 100 4242 4242 34816 4250 ...",
			wantPPID:  100,
			wantTpgid: 4250,
		},
		{
			name:      "comm with spaces",
			line:      "4242 (my long name) S 100 4242 4242 34816 4250 0 0",
			wantPPID:  100,
			wantTpgid: 4250,
		},
		{
			name:      "comm with embedded parens (split on LAST paren)",
			line:      "4242 (we)ird) S 7 8 9 10 4250 0",
			wantPPID:  7,
			wantTpgid: 4250,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ok := parseStatAfterComm(tc.line)
			if !ok {
				t.Fatalf("parse failed for %q", tc.line)
			}
			if len(f) < 6 {
				t.Fatalf("too few fields: %v", f)
			}
			if f[1] != strconv.Itoa(tc.wantPPID) {
				t.Errorf("ppid field = %q, want %d", f[1], tc.wantPPID)
			}
			if f[5] != strconv.Itoa(tc.wantTpgid) {
				t.Errorf("tpgid field = %q, want %d", f[5], tc.wantTpgid)
			}
		})
	}

	// Malformed inputs degrade to (nil,false), not a panic.
	for _, bad := range []string{"", "no parens here", "trailing)"} {
		if _, ok := parseStatAfterComm(bad); ok {
			t.Errorf("parseStatAfterComm(%q) = ok, want not-ok", bad)
		}
	}
}

// ppidOf against this test process must match the real getppid — the
// end-to-end check that statAfterComm + field indexing line up with the
// kernel's actual /proc layout (guards an off-by-one in the field map).
func TestPpidOfSelfMatchesGetppid(t *testing.T) {
	t.Parallel()
	got, ok := ppidOf(os.Getpid())
	if !ok {
		t.Fatal("ppidOf(self) failed")
	}
	if want := os.Getppid(); got != want {
		t.Errorf("ppidOf(self) = %d, want getppid %d", got, want)
	}
}

// ResolveForegroundComm rejects non-positive pgids without touching
// /proc (the confinement + sanitize behavior itself is covered by the
// ptysidecar fg tests).
func TestResolveForegroundCommGuards(t *testing.T) {
	t.Parallel()
	if got := ptysidecar.ResolveForegroundComm(0, 1234); got != "" {
		t.Errorf("pgid 0 = %q, want empty", got)
	}
	if got := ptysidecar.ResolveForegroundComm(-1, 1234); got != "" {
		t.Errorf("negative pgid = %q, want empty", got)
	}
}
