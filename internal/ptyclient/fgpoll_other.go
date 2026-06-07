//go:build !linux

package ptyclient

// runFgFallbackPoller is a no-op off Linux: the daemon-side fg fallback
// resolves from /proc, and the sidecar fg path is Linux-only too.
func (c *Conn) runFgFallbackPoller() {}
