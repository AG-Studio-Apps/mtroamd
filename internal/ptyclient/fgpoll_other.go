//go:build !linux && !darwin

package ptyclient

// Stub leaf lookups for platforms without an fg implementation (e.g.
// freebsd). The shared poller loop (fgpoll.go) calls these; returning
// false makes it resolve the shell once, find nothing, and go idle —
// a cheap no-op. (The sidecar fg path is likewise stubbed there.)

func sessionShellPID(_ int) (int, bool) { return 0, false }

func tpgidOf(_ int) (int, bool) { return 0, false }
