//go:build !linux

package ptysidecar

import "os"

// foregroundComm is Linux-only for now (/proc + TIOCGPGRP). Other
// platforms report "" — the poller then never observes a change and
// FrameFgState is never emitted (zero cost, field simply absent).
// macOS/BSD support would use libproc/sysctl equivalents.
func foregroundComm(_ *os.File, _ int) string { return "" }

// ResolveForegroundComm is the non-Linux stub for the shared resolver
// (see the Linux build). The daemon's fallback poller is Linux-only.
func ResolveForegroundComm(_, _ int) string { return "" }
