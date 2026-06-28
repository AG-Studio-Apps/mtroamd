// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
)

type interfaceEntry struct {
	Name string
	IP   net.IP
}

// ErrNoTailnetInterface is returned by ResolveTailnetBindIP when no
// local interface has an address in a Tailscale range. Callers
// should surface this verbatim — operators read it in unit-start
// failure logs.
var ErrNoTailnetInterface = errors.New("no local interface has a Tailscale address (is Tailscale running?)")

// ResolveTailnetBindIP walks the host's network interfaces and
// returns the first IP that falls inside one of Tailscale's
// well-known address ranges, preferring the canonical tailscale0
// interface on Linux to avoid CGNAT address collisions.
//
// Two-pass resolution:
//
//  1. Prefer IPs on "tailscale0" — Linux's unambiguous Tailscale
//     interface. This eliminates false positives from ISP/VPN/
//     container interfaces that also use 100.64.0.0/10 (RFC 6598).
//  2. Fall back to the first Tailscale-range IP on any interface.
//     Needed for macOS (utunN — shared name, can't filter) and
//     embedded tsnet.
//
// Cross-platform: works identically on Linux and macOS via the
// standard library's net.Interfaces(). Doesn't shell out to the
// `tailscale` CLI, so it's robust against PATH quirks and works
// regardless of how Tailscale was installed (system app, CLI-only,
// embedded tsnet binary).
//
// Returns ErrNoTailnetInterface if no tailnet address is found.
func ResolveTailnetBindIP() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate interfaces: %w", err)
	}
	var entries []interfaceEntry
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			entries = append(entries, interfaceEntry{
				Name: ifc.Name,
				IP:   ipnet.IP,
			})
		}
	}
	return resolveTailnetIP(entries)
}

func resolveTailnetIP(entries []interfaceEntry) (net.IP, error) {
	for _, e := range entries {
		if e.Name == "tailscale0" && IsTailnetIP(e.IP) {
			slog.Debug("tailnet bind: resolved via tailscale0",
				"ip", e.IP.String(), "iface", e.Name)
			return e.IP, nil
		}
	}
	for _, e := range entries {
		if IsTailnetIP(e.IP) {
			slog.Debug("tailnet bind: resolved via fallback (no tailscale0)",
				"ip", e.IP.String(), "iface", e.Name)
			return e.IP, nil
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

// guardPlaintextBind vets a bind IP for the un-TLS'd TCP listener. That
// transport carries the session protocol in cleartext, relying on
// tailnet/loopback/LAN confinement — the QUIC listener is the TLS-bearing
// path for any wider exposure. We therefore:
//
//   - allow loopback, RFC1918-private, link-local, and tailnet addresses
//     (confined to a trusted network) silently;
//   - FAIL CLOSED on an unspecified bind (0.0.0.0 / :: / host-less ":port"),
//     which exposes the cleartext protocol on EVERY interface (including
//     non-tailnet/public ones); the operator must instead bind a
//     loopback/private/tailnet address (or "tailnet:<port>"), or explicitly
//     opt in via MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED;
//   - refuse a concrete globally-routable address, where plaintext exposure
//     would be unambiguous.
func guardPlaintextBind(ip net.IP) error {
	if ip == nil || ip.IsUnspecified() {
		addr := "(unspecified)"
		if ip != nil {
			addr = ip.String()
		}
		if !plaintextUnspecifiedAllowed() {
			return fmt.Errorf("refusing to bind the plaintext (un-TLS'd) TCP listener to unspecified address %s: "+
				"it would expose the cleartext terminal protocol (and attach tokens) on every interface. "+
				"Bind a loopback/private/tailnet address (or \"tailnet:<port>\"), use the QUIC listener (TLS 1.3) "+
				"for public exposure, or set MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED=1 to override", addr)
		}
		slog.Warn("transport: plaintext (un-TLS'd) TCP listener bound to an unspecified address via "+
			"MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED override; ensure it is firewalled to a tailnet/LAN",
			"addr", addr)
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || IsTailnetIP(ip) {
		return nil
	}
	return fmt.Errorf("refusing to bind the plaintext (un-TLS'd) TCP listener to globally-routable address %s: "+
		"bind a loopback/private/tailnet address, or use the QUIC listener (TLS 1.3) for public exposure", ip.String())
}

// plaintextUnspecifiedAllowed reports whether the operator has explicitly opted
// into binding the un-TLS'd TCP listener to an unspecified address (every
// interface). Off by default so a misconfiguration fails closed rather than
// silently leaking cleartext terminal traffic.
func plaintextUnspecifiedAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ResolveBindAddr expands the "tailnet:<port>" sentinel into a
// concrete "<tailnet-ip>:<port>" bind address using
// ResolveTailnetBindIP. Any other input is returned unchanged — the
// daemon's serve loop calls this once at startup so operators can
// still pass fully-specified host:port pairs. Note an unspecified bind
// ("0.0.0.0:N" / ":N") is now refused by guardPlaintextBind unless
// MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED is set.
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
