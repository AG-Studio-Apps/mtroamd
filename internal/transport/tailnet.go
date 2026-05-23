// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"errors"
	"fmt"
	"net"
)

// ErrNoTailnetInterface is returned by ResolveTailnetBindIP when no
// local interface has an address in a Tailscale range. Callers
// should surface this verbatim — operators read it in unit-start
// failure logs.
var ErrNoTailnetInterface = errors.New("no local interface has a Tailscale address (is Tailscale running?)")

// ResolveTailnetBindIP walks the host's local interfaces and returns
// the first IP that falls inside one of Tailscale's well-known
// address ranges:
//
//   - IPv4 100.64.0.0/10 — Tailscale's CGNAT block
//   - IPv6 fd7a:115c:a1e0::/48 — Tailscale's ULA prefix
//
// Used at daemon startup to honour the "bind TCP to tailnet only"
// posture: the Roam-over-TCP listener runs unencrypted, trusting
// WireGuard for confidentiality, so the port MUST NOT be exposed on
// any non-Tailscale interface. Binding to this resolved IP enforces
// that at the kernel layer.
//
// Cross-platform: works identically on Linux and macOS via the
// standard library's net.InterfaceAddrs(). Doesn't shell out to the
// `tailscale` CLI, so it's robust against PATH quirks and works
// regardless of how Tailscale was installed (system app, CLI-only,
// embedded tsnet binary).
//
// Returns ErrNoTailnetInterface if no tailnet address is found.
// Operators who hit that error need to bring up Tailscale first
// (or, if they specifically need a non-Tailscale bind, pass an
// explicit IP to --roam-tcp-addr rather than the "tailnet:" prefix).
func ResolveTailnetBindIP() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("enumerate interfaces: %w", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if IsTailnetIP(ipnet.IP) {
			return ipnet.IP, nil
		}
	}
	return nil, ErrNoTailnetInterface
}

// IsTailnetIP reports whether ip falls in a Tailscale-assigned
// range. Public so tests + callers can spot-check arbitrary IPs;
// also used by ResolveTailnetBindIP internally. The byte literals
// are the Tailscale architecture's well-known prefixes — they do
// not depend on a specific deployment.
func IsTailnetIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// IPv4 100.64.0.0/10 — first octet 100, second in [64, 127].
	if ip4 := ip.To4(); ip4 != nil {
		return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
	}
	// IPv6 fd7a:115c:a1e0::/48 — first 6 bytes pin the /48 prefix.
	if len(ip) == net.IPv6len {
		return ip[0] == 0xfd &&
			ip[1] == 0x7a &&
			ip[2] == 0x11 &&
			ip[3] == 0x5c &&
			ip[4] == 0xa1 &&
			ip[5] == 0xe0
	}
	return false
}

// ResolveBindAddr expands the "tailnet:<port>" sentinel into a
// concrete "<tailnet-ip>:<port>" bind address using
// ResolveTailnetBindIP. Any other input is returned unchanged — the
// daemon's serve loop calls this once at startup so operators can
// still pass fully-specified host:port pairs (including "0.0.0.0:N"
// with the documented security caveat).
//
// Returns the resolved address and the resolved IP (or nil + the
// original addr if no resolution happened). Errors from the resolver
// propagate so the daemon can fail loud rather than silently falling
// back to a less-safe bind.
func ResolveBindAddr(addr string) (string, net.IP, error) {
	const sentinel = "tailnet:"
	if len(addr) < len(sentinel) || addr[:len(sentinel)] != sentinel {
		return addr, nil, nil
	}
	port := addr[len(sentinel):]
	if port == "" {
		return "", nil, fmt.Errorf("invalid bind address %q: missing port after %q", addr, sentinel)
	}
	ip, err := ResolveTailnetBindIP()
	if err != nil {
		return "", nil, fmt.Errorf("resolve tailnet bind IP for %q: %w", addr, err)
	}
	// IPv6 needs bracketed host:port form for net.Listen.
	if ip.To4() == nil {
		return fmt.Sprintf("[%s]:%s", ip.String(), port), ip, nil
	}
	return fmt.Sprintf("%s:%s", ip.String(), port), ip, nil
}
