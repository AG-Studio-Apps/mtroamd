//go:build linux

package main

import (
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
)

// makeSessionOOMPreferred should raise the current process's oom_score_adj
// strictly ABOVE its inherited baseline (by the bump, capped at the max), so a
// runaway session (not the daemon) is the OOM victim. Verified relative, not
// absolute, because the user-session baseline is non-zero (systemd user manager
// defaults it to 200 on Ubuntu).
func TestMakeSessionOOMPreferredRaisesAboveInherited(t *testing.T) {
	const p = "/proc/self/oom_score_adj"
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("no %s on this platform: %v", p, err)
	}
	baseline, err := strconv.Atoi(strings.TrimSpace(string(orig)))
	if err != nil {
		t.Skipf("unparseable %s: %q", p, string(orig))
	}
	t.Cleanup(func() { _ = os.WriteFile(p, orig, 0o644) })

	makeSessionOOMPreferred(slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back %s: %v", p, err)
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(got)))

	want := baseline + sessionOOMScoreBump
	if want > oomScoreAdjMax {
		want = oomScoreAdjMax
	}
	if v == baseline && baseline < oomScoreAdjMax {
		t.Skipf("oom_score_adj unchanged (%d): sandbox likely denied the write", v)
	}
	if v != want {
		t.Errorf("oom_score_adj = %d, want %d (baseline %d + bump %d)", v, want, baseline, sessionOOMScoreBump)
	}
	if v <= baseline && baseline < oomScoreAdjMax {
		t.Errorf("session not raised above its inherited baseline %d", baseline)
	}
}
