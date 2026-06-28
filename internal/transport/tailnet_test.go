// Copyright (c) AG-Studio Apps & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package transport

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestIsTailnetIP(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		// IPv4 CGNAT 100.64.0.0/10
		{"v4 cgnat lower edge", "100.64.0.0", true},
		{"v4 cgnat upper edge", "100.127.255.255", true},
		{"v4 cgnat mid", "100.120.158.38", true},
		{"v4 just below cgnat", "100.63.255.255", false},
		{"v4 just above cgnat", "100.128.0.0", false},
		{"v4 unrelated 100.x.x.x", "100.5.1.1", false},
		{"v4 RFC1918", "192.168.1.1", false},
		{"v4 loopback", "127.0.0.1", false},
		{"v4 public", "8.8.8.8", false},
		// IPv6 ULA fd7a:115c:a1e0::/48
		{"v6 tailnet prefix", "fd7a:115c:a1e0::1", true},
		{"v6 tailnet last in prefix", "fd7a:115c:a1e0:ffff:ffff:ffff:ffff:ffff", true},
		{"v6 ULA but not tailscale", "fdfe:dcba:9876::1", false},
		{"v6 loopback", "::1", false},
		{"v6 public", "2001:4860:4860::8888", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsTailnetIP(net.ParseIP(c.ip))
			if got != c.want {
				t.Errorf("IsTailnetIP(%q) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
	t.Run("nil ip", func(t *testing.T) {
		if IsTailnetIP(nil) {
			t.Error("IsTailnetIP(nil) = true, want false")
		}
	})
}

func TestResolveTailnetIP(t *testing.T) {
	cases := []struct {
		name    string
		entries []interfaceEntry
		wantIP  string // empty means expect error
	}{
		{
			"prefers tailscale0 over other CGNAT",
			[]interfaceEntry{
				{"eth0", net.ParseIP("100.64.0.1")},
				{"tailscale0", net.ParseIP("100.100.1.1")},
			},
			"100.100.1.1",
		},
		{
			"CGNAT collision: docker bridge ignored when tailscale0 present",
			[]interfaceEntry{
				{"docker0", net.ParseIP("100.64.0.1")},
				{"tailscale0", net.ParseIP("100.100.1.2")},
			},
			"100.100.1.2",
		},
		{
			"fallback when no tailscale0 (macOS utun)",
			[]interfaceEntry{
				{"utun3", net.ParseIP("100.100.1.3")},
			},
			"100.100.1.3",
		},
		{
			"fallback IPv6",
			[]interfaceEntry{
				{"utun5", net.ParseIP("fd7a:115c:a1e0::1")},
			},
			"fd7a:115c:a1e0::1",
		},
		{
			"tailscale0 with IPv6",
			[]interfaceEntry{
				{"tailscale0", net.ParseIP("fd7a:115c:a1e0::1")},
			},
			"fd7a:115c:a1e0::1",
		},
		{
			"tailscale0 has non-tailnet IP: falls back to other iface",
			[]interfaceEntry{
				{"tailscale0", net.ParseIP("192.168.1.1")},
				{"utun3", net.ParseIP("100.100.1.5")},
			},
			"100.100.1.5",
		},
		{
			"no match",
			[]interfaceEntry{
				{"eth0", net.ParseIP("192.168.1.1")},
				{"wlan0", net.ParseIP("10.0.0.5")},
			},
			"",
		},
		{
			"empty entries",
			nil,
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveTailnetIP(c.entries)
			if c.wantIP == "" {
				if !errors.Is(err, ErrNoTailnetInterface) {
					t.Fatalf("err = %v, want ErrNoTailnetInterface", err)
				}
				if got != nil {
					t.Fatalf("ip = %v, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != c.wantIP {
				t.Errorf("ip = %s, want %s", got, c.wantIP)
			}
		})
	}
}

func TestResolveBindAddr_Passthrough(t *testing.T) {
	// Non-sentinel inputs are returned untouched without invoking
	// the tailnet resolver — so the daemon honours operators who
	// pass explicit host:port pairs (including the "0.0.0.0:N"
	// override).
	cases := []string{
		"0.0.0.0:49821",
		"127.0.0.1:49821",
		"100.64.0.1:49821",
		"[fd7a:115c:a1e0::1]:49821",
		"", // empty also passes through
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got, ip, err := ResolveBindAddr(in)
			if err != nil {
				t.Fatalf("ResolveBindAddr(%q) err = %v", in, err)
			}
			if got != in {
				t.Errorf("ResolveBindAddr(%q) = %q, want %q", in, got, in)
			}
			if ip != nil {
				t.Errorf("ResolveBindAddr(%q) ip = %v, want nil", in, ip)
			}
		})
	}
}

func TestResolveBindAddr_MissingPort(t *testing.T) {
	// "tailnet:" with no port is operator error; surface it instead
	// of silently producing "100.x.x.x:".
	_, _, err := ResolveBindAddr("tailnet:")
	if err == nil {
		t.Fatal("ResolveBindAddr(\"tailnet:\") err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "missing port") {
		t.Errorf("err = %v, want one mentioning 'missing port'", err)
	}
}

// TestResolveBindAddr_NoTailnet exercises the unhappy path. Most CI
// runners have no Tailscale interface, so the sentinel resolution
// surfaces ErrNoTailnetInterface — exactly the failure the daemon
// should propagate to its caller (rather than silently binding to
// 0.0.0.0 and exposing the port).
//
// Hosts that DO have Tailscale up (a developer running tests on a
// tailnet-joined box) will see this test skip, since the resolver
// returns success. We assert only on the structured outcome.
func TestResolveBindAddr_NoTailnet(t *testing.T) {
	addr, ip, err := ResolveBindAddr("tailnet:49821")
	if err != nil {
		if !errors.Is(err, ErrNoTailnetInterface) {
			t.Fatalf("err = %v, want ErrNoTailnetInterface wrap", err)
		}
		if addr != "" {
			t.Errorf("on err, addr = %q, want empty", addr)
		}
		if ip != nil {
			t.Errorf("on err, ip = %v, want nil", ip)
		}
		return
	}
	// Tailscale was up on this host — resolution succeeded. Sanity-
	// check the resolved address looks like one of our prefixes.
	if !IsTailnetIP(ip) {
		t.Errorf("resolved ip %v not in any tailnet range", ip)
	}
	if !strings.HasSuffix(addr, ":49821") {
		t.Errorf("resolved addr %q doesn't keep the requested port", addr)
	}
}

func TestGuardPlaintextBind(t *testing.T) {
	allowed := []net.IP{
		net.ParseIP("127.0.0.1"),         // loopback
		net.ParseIP("::1"),               // loopback v6
		net.ParseIP("10.0.0.5"),          // RFC1918
		net.ParseIP("192.168.1.20"),      // RFC1918
		net.ParseIP("172.16.3.4"),        // RFC1918
		net.ParseIP("169.254.1.1"),       // link-local
		net.ParseIP("100.96.0.1"),        // tailnet (100.64/10)
		net.ParseIP("fd7a:115c:a1e0::1"), // tailnet v6
	}
	for _, ip := range allowed {
		if err := guardPlaintextBind(ip); err != nil {
			t.Errorf("guardPlaintextBind(%v) = %v, want allowed (nil)", ip, err)
		}
	}

	// Unspecified / host-less binds FAIL CLOSED by default: they'd expose the
	// cleartext protocol on every interface, so they're refused without an
	// explicit opt-in.
	unspecified := []net.IP{nil, net.IPv4zero, net.IPv6unspecified}
	for _, ip := range unspecified {
		if err := guardPlaintextBind(ip); err == nil {
			t.Errorf("guardPlaintextBind(%v) = nil, want refusal (fail-closed)", ip)
		}
	}

	// Concrete globally-routable addresses are refused.
	refused := []net.IP{
		net.ParseIP("8.8.8.8"),
		net.ParseIP("1.1.1.1"),
		net.ParseIP("203.0.113.7"),
		net.ParseIP("2606:4700:4700::1111"),
	}
	for _, ip := range refused {
		if err := guardPlaintextBind(ip); err == nil {
			t.Errorf("guardPlaintextBind(%v) = nil, want refusal", ip)
		}
	}

	// With the explicit override, unspecified binds are permitted (warn only).
	t.Setenv("MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED", "1")
	for _, ip := range unspecified {
		if err := guardPlaintextBind(ip); err != nil {
			t.Errorf("guardPlaintextBind(%v) with override = %v, want permitted (nil)", ip, err)
		}
	}
	// The override does NOT loosen a concrete globally-routable refusal.
	if err := guardPlaintextBind(net.ParseIP("8.8.8.8")); err == nil {
		t.Error("guardPlaintextBind(8.8.8.8) with override = nil, want refusal")
	}
}
