package main

import "testing"

// The daemon's Status reports build.String() - "vX.Y.Z (sha, built ...)"
// - NOT the bare tag. versionToken must reduce it to the comparable
// form; comparing the full string never matches and would fail every
// update's post-restart verification (caught in pre-release testing).
func TestVersionTokenReducesStatusString(t *testing.T) {
	cases := map[string]string{
		"v1.7.8-rc2 (ee2fdb2, built 2026-07-21T18:52:19Z)": "v1.7.8-rc2",
		"v1.7.8-rc2": "v1.7.8-rc2",
		"":           "",
	}
	for in, want := range cases {
		if got := versionToken(in); got != want {
			t.Errorf("versionToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// The census must never include the calling process itself (doctor/
// update run from the same binary name) and every entry must be a real
// pid. On CI no daemon runs, so an empty result is fine.
func TestFindServeProcessesExcludesSelf(t *testing.T) {
	for _, p := range findServeProcesses() {
		if p.PID <= 0 {
			t.Errorf("census entry with bogus pid: %+v", p)
		}
	}
}
