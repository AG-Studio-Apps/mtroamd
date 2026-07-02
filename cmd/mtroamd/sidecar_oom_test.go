//go:build linux

package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// makeSessionOOMPreferred should raise the current process's oom_score_adj to
// the session value so a runaway session — not the daemon — is the OOM victim.
func TestMakeSessionOOMPreferredSetsScore(t *testing.T) {
	const p = "/proc/self/oom_score_adj"
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("no %s on this platform: %v", p, err)
	}
	// Probe writability; a constrained sandbox may deny it — skip rather than fail.
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Skipf("cannot write %s (sandbox?): %v", p, err)
	}
	t.Cleanup(func() { _ = os.WriteFile(p, orig, 0o644) })

	makeSessionOOMPreferred(slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back %s: %v", p, err)
	}
	if strings.TrimSpace(string(got)) != sessionOOMScoreAdj {
		t.Errorf("oom_score_adj = %q, want %q", strings.TrimSpace(string(got)), sessionOOMScoreAdj)
	}
}
