package svcmgr

import (
	"fmt"
	"strings"
)

// UserUnitOptions is the input to RenderUserUnit. Zero-valued fields
// fall back to the defaults a fresh install would use, so callers
// that just want the canonical file pass &UserUnitOptions{} or nil.
type UserUnitOptions struct {
	// BinPath is the absolute path to the daemon binary referenced
	// from ExecStart. Defaults to "%h/.local/bin/mtroamd" so the
	// unit file is portable across users on the same host (systemd
	// expands %h at unit-load time).
	BinPath string

	// Addr is the QUIC bind address (host:port). Defaults to
	// "0.0.0.0:49820" — chosen to avoid WireGuard (51820),
	// Tailscale (41641), and other well-known UDP services. The
	// daemon will fall through to the next free port in the
	// 49820–49919 range if the preferred port is in use, and
	// persist the bound port across restarts. Operators can pass
	// an explicit override (e.g. a Tailnet IP, or a different port
	// they've decided on) — explicit non-default ports are honoured
	// strictly without stickiness override.
	Addr string

	// SocketPath is the IPC socket the daemon binds for `connect`.
	// Defaults to "%h/.local/share/mtroamd/mtroamd.sock" matching
	// the default-path lookup in `cmd/mtroamd/connect.go`.
	SocketPath string

	// TCPAddr is the optional mtRoam-over-TCP listener bind address
	// (used by iOS clients in embedded-Tailscale routing mode).
	// Defaults to "tailnet:49920" — the daemon resolves the local
	// tailnet IP at startup and binds there. The unencrypted TCP
	// path trusts WireGuard for confidentiality, so it MUST NOT be
	// exposed on non-tailnet interfaces; binding to a resolved
	// tailnet IP enforces that at the kernel layer. If Tailscale
	// isn't running at startup the daemon refuses to bind the TCP
	// listener and logs an explicit error — failing loud beats
	// silently exposing plaintext on a public interface.
	//
	// Operators with a reason to bypass that posture (a TLS-
	// terminating reverse proxy in front, a non-Tailscale private
	// network) can pass an explicit "host:port" pair. The sentinel
	// "-" suppresses the --mtroam-tcp-addr flag entirely (QUIC-only).
	TCPAddr string
}

// RenderUserUnit returns the canonical `~/.config/systemd/user/
// mtroamd.service` content. This is THE source of truth for the
// systemd-user unit shipped by the project:
//   - `mtroamd unit print` writes this to stdout
//   - the iOS auto-installer's SystemdUnitTemplate is kept byte-
//     identical via a snapshot test
//   - AUR/Homebrew packaging pipes this into the package payload
//
// Edits here propagate to every install path. `KillMode=process` is
// load-bearing for v0.6+: it preserves pty-sidecar children across
// daemon restart (the default control-group kill wipes the whole
// cgroup including sidecars + their child shells).
//
// The Memory* ceilings are equally load-bearing, for the opposite
// reason: KillMode=process means anything that escapes a session
// survives every unit cycle and accumulates for the life of the boot,
// so the cgroup is the only thing bounding the damage.
func RenderUserUnit(opts *UserUnitOptions) string {
	o := UserUnitOptions{}
	if opts != nil {
		o = *opts
	}
	if o.BinPath == "" {
		o.BinPath = "%h/.local/bin/mtroamd"
	}
	if o.Addr == "" {
		o.Addr = "0.0.0.0:49820"
	}
	if o.SocketPath == "" {
		o.SocketPath = "%h/.local/share/mtroamd/mtroamd.sock"
	}
	if o.TCPAddr == "" {
		o.TCPAddr = "tailnet:49920"
	}

	var b strings.Builder
	fmt.Fprintln(&b, "[Unit]")
	fmt.Fprintln(&b, "Description=mtroamd — meshTerm roaming daemon")
	fmt.Fprintln(&b, "Documentation=https://github.com/AG-Studio-Apps/mtroamd")
	fmt.Fprintln(&b, "After=network.target")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Service]")
	fmt.Fprintln(&b, "Type=simple")
	// Operator opt-out: "-" suppresses the --mtroam-tcp-addr flag for
	// the rare case someone wants QUIC-only. Everything else emits
	// both transports so an iOS-side embedded-mode switch needs no
	// re-install.
	if o.TCPAddr == "-" {
		fmt.Fprintf(&b, "ExecStart=%s serve --addr %s --socket %s\n", o.BinPath, o.Addr, o.SocketPath)
	} else {
		fmt.Fprintf(&b, "ExecStart=%s serve --addr %s --mtroam-tcp-addr %s --socket %s\n",
			o.BinPath, o.Addr, o.TCPAddr, o.SocketPath)
	}
	fmt.Fprintln(&b, "Restart=on-failure")
	fmt.Fprintln(&b, "RestartSec=5")
	fmt.Fprintln(&b, "# KillMode=process so `systemctl restart` only SIGTERMs the main")
	fmt.Fprintln(&b, "# daemon — the per-session pty-sidecar children survive in their")
	fmt.Fprintln(&b, "# cgroup waiting for the new daemon to dial them back. The")
	fmt.Fprintln(&b, "# default (control-group) wipes every sidecar + child shell on")
	fmt.Fprintln(&b, "# unit cycle, defeating v0.6.0's restart-resilient PTY split.")
	fmt.Fprintln(&b, "KillMode=process")
	fmt.Fprintln(&b, "# Cgroup ceilings. Everything the daemon owns shares this cgroup -")
	fmt.Fprintln(&b, "# the daemon, every session's pty-sidecar, their child shells, and")
	fmt.Fprintln(&b, "# any agent (Claude/codex/agy) a user runs inside one. Uncapped,")
	fmt.Fprintln(&b, "# a runaway or a leak walks the whole box into swap thrash and the")
	fmt.Fprintln(&b, "# machine stops responding WITHOUT the OOM killer ever firing (box")
	fmt.Fprintln(&b, "# freeze 2026-08-06: 5.0G resident + 4.0G swap on a 7.7G host,")
	fmt.Fprintln(&b, "# oom_kill count 0). The v1.7.3 per-sidecar oom_score_adj only picks")
	fmt.Fprintln(&b, "# WHO dies once the kernel decides someone must; it cannot stop the")
	fmt.Fprintln(&b, "# thrash. These do:")
	fmt.Fprintln(&b, "#   MemoryHigh    soft throttle - reclaim pressure starts here")
	fmt.Fprintln(&b, "#   MemoryMax     hard cap - the in-cgroup OOM killer takes ONE")
	fmt.Fprintln(&b, "#                 process (a session), never the whole box")
	fmt.Fprintln(&b, "#   MemorySwapMax the load-bearing one - a small swap budget is what")
	fmt.Fprintln(&b, "#                 forces a prompt in-cgroup kill instead of hours of")
	fmt.Fprintln(&b, "#                 thrashing. It takes no percentage, so it is absolute.")
	fmt.Fprintln(&b, "# The percentages are of installed RAM, so these are sane on a 2G VPS")
	fmt.Fprintln(&b, "# and a 64G workstation alike. Override with a drop-in")
	fmt.Fprintln(&b, "# (~/.config/systemd/user/mtroamd.service.d/*.conf) - drop-ins survive")
	fmt.Fprintln(&b, "# `mtroamd migrate` and reinstall, edits to this file do not.")
	fmt.Fprintln(&b, "MemoryHigh=40%")
	fmt.Fprintln(&b, "MemoryMax=55%")
	fmt.Fprintln(&b, "MemorySwapMax=512M")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Install]")
	fmt.Fprintln(&b, "WantedBy=default.target")
	return b.String()
}
