//go:build !linux

package ptysidecar

import "os"

// foregroundComm is Linux-only for now (/proc + TIOCGPGRP). Other
// platforms report "" — the poller then never observes a change and
// FrameFgState is never emitted (zero cost, field simply absent).
// macOS/BSD support would use libproc/sysctl equivalents.
func foregroundComm(_ *os.File) string { return "" }
