package transport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPortStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writePortState(dir, 49823)
	got := readPortState(dir)
	if got != 49823 {
		t.Errorf("readPortState after write: got %d, want 49823", got)
	}
}

func TestReadPortStateMissingFile(t *testing.T) {
	dir := t.TempDir()
	// No write — file doesn't exist.
	if got := readPortState(dir); got != 0 {
		t.Errorf("missing file: got %d, want 0", got)
	}
}

func TestReadPortStateEmptyStateDir(t *testing.T) {
	// Empty stateDir means stickiness disabled.
	if got := readPortState(""); got != 0 {
		t.Errorf("empty stateDir: got %d, want 0", got)
	}
}

func TestReadPortStateUnparseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, quicPortStateFile)
	if err := os.WriteFile(path, []byte("not a number"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := readPortState(dir); got != 0 {
		t.Errorf("unparseable: got %d, want 0", got)
	}
}

func TestReadPortStateZero(t *testing.T) {
	// A file containing "0" should be treated as no preference,
	// matching the missing-file case. Otherwise port 0 (the OS
	// ephemeral allocator) would shadow stickiness logic.
	dir := t.TempDir()
	path := filepath.Join(dir, quicPortStateFile)
	if err := os.WriteFile(path, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := readPortState(dir); got != 0 {
		t.Errorf("zero literal: got %d, want 0", got)
	}
}

func TestWritePortStateBestEffort(t *testing.T) {
	// Writing to a non-existent stateDir should be a no-op
	// (best-effort: log and continue). Bare write to "" must not
	// panic and must not create anything.
	writePortState("", 49820)
	// No assertion: success criterion is "didn't panic".
}

func TestWritePortStateCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dirs", "mtroamd")
	writePortState(dir, 49830)
	// Should have created the dir tree and written the file.
	if got := readPortState(dir); got != 49830 {
		t.Errorf("after writing into nested dir: got %d, want 49830", got)
	}
}

func TestBuildCandidatePortsHappyPath(t *testing.T) {
	// No stickiness — straight walk from prefPort.
	got := buildCandidatePorts(DefaultQUICPort, 0)
	if len(got) != int(FallbackPortSpan)+1 {
		t.Fatalf("len = %d, want %d", len(got), FallbackPortSpan+1)
	}
	if got[0] != DefaultQUICPort {
		t.Errorf("first candidate: got %d, want %d", got[0], DefaultQUICPort)
	}
	if got[1] != DefaultQUICPort+1 {
		t.Errorf("second candidate: got %d, want %d", got[1], DefaultQUICPort+1)
	}
	if got[len(got)-1] != DefaultQUICPort+FallbackPortSpan {
		t.Errorf("last candidate: got %d, want %d",
			got[len(got)-1], DefaultQUICPort+FallbackPortSpan)
	}
}

func TestBuildCandidatePortsStickinessPrefersPersisted(t *testing.T) {
	// stuck=49823 + default prefPort means stuck should be tried first.
	got := buildCandidatePorts(DefaultQUICPort, 49823)
	if got[0] != 49823 {
		t.Errorf("with stuck=49823, first candidate: got %d, want 49823", got[0])
	}
	// stuck shouldn't appear twice — the walk through prefPort..+span
	// must skip it.
	count := 0
	for _, p := range got {
		if p == 49823 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("stuck port 49823 appears %d times, want exactly 1", count)
	}
}

func TestBuildCandidatePortsStickinessIgnoredForNonDefault(t *testing.T) {
	// User explicitly configured port 60000 (non-default). State
	// file says 49823. We must NOT honour stickiness — explicit
	// config wins. Walk should start at 60000.
	got := buildCandidatePorts(60000, 49823)
	if got[0] != 60000 {
		t.Errorf("with non-default pref=60000 + stuck=49823, first: got %d, want 60000", got[0])
	}
	// stuck shouldn't appear at all — it's outside the walk range.
	for _, p := range got {
		if p == 49823 {
			t.Errorf("stuck port 49823 should not appear when prefPort != default; candidates=%v", got)
			break
		}
	}
}

func TestBuildCandidatePortsStickinessSameAsPref(t *testing.T) {
	// stuck == prefPort: no special-case ordering needed.
	got := buildCandidatePorts(DefaultQUICPort, DefaultQUICPort)
	if got[0] != DefaultQUICPort {
		t.Errorf("stuck == pref: first candidate: got %d, want %d", got[0], DefaultQUICPort)
	}
	// The default port should appear exactly once, not twice.
	count := 0
	for _, p := range got {
		if p == DefaultQUICPort {
			count++
		}
	}
	if count != 1 {
		t.Errorf("default port appears %d times, want exactly 1", count)
	}
}

// TestBuildCandidatePortsStickinessOutOfRange covers the migration
// case where a `quic-port` file written by a pre-v1.1.6 daemon (whose
// default UDP port was 51820, well outside the v1.2.x walk range
// 49820..49919) survives across upgrades. The stickiness layer must
// reject the out-of-range port and walk the configured range
// normally; otherwise the old default keeps winning indefinitely
// even after the architectural default moves.
func TestBuildCandidatePortsStickinessOutOfRange(t *testing.T) {
	cases := []struct {
		name  string
		stuck uint16
	}{
		{"pre-v1.1.6 default (51820)", 51820},
		{"just above span", DefaultQUICPort + FallbackPortSpan + 1},
		{"way below prefPort", 1024},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildCandidatePorts(DefaultQUICPort, c.stuck)
			if got[0] != DefaultQUICPort {
				t.Errorf("out-of-range stuck=%d should not lead candidate list; got first=%d, want %d",
					c.stuck, got[0], DefaultQUICPort)
			}
			for _, p := range got {
				if p == c.stuck {
					t.Errorf("out-of-range stuck=%d leaked into candidates: %v", c.stuck, got)
					break
				}
			}
			if uint16(len(got)) != FallbackPortSpan+1 {
				t.Errorf("with stuck out-of-range, expected %d candidates (walk range only), got %d",
					FallbackPortSpan+1, len(got))
			}
		})
	}
}

// TestBuildCandidatePortsStickinessUpperEdge: a stuck port at exactly
// the top of the walk range is still legitimate and should be tried
// first. Pinning the inclusive-upper-bound semantics.
func TestBuildCandidatePortsStickinessUpperEdge(t *testing.T) {
	stuck := DefaultQUICPort + FallbackPortSpan
	got := buildCandidatePorts(DefaultQUICPort, stuck)
	if got[0] != stuck {
		t.Errorf("upper-edge stuck=%d should lead; got first=%d", stuck, got[0])
	}
	count := 0
	for _, p := range got {
		if p == stuck {
			count++
		}
	}
	if count != 1 {
		t.Errorf("upper-edge stuck=%d appears %d times, want 1", stuck, count)
	}
}
