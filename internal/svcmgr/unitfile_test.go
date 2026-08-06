package svcmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderUserUnitGolden pins the byte-for-byte output of the
// canonical user unit so any change goes through code review with a
// visible diff. The string is intentionally embedded inline (rather
// than read from a testdata file) so a passing test guarantees this
// file matches the regenerated output AND so reviewers see the
// expected content right next to the producing function.
//
// If you intentionally change the unit format, update both this
// constant AND the iOS-side SystemdUnitTemplate so the two stay
// byte-identical. The iOS template's own snapshot test (if added)
// would surface the drift.
func TestRenderUserUnitGolden(t *testing.T) {
	const want = `[Unit]
Description=mtroamd — meshTerm roaming daemon
Documentation=https://github.com/AG-Studio-Apps/mtroamd
After=network.target

[Service]
Type=simple
ExecStart=%h/.local/bin/mtroamd serve --addr 0.0.0.0:49820 --mtroam-tcp-addr tailnet:49920 --socket %h/.local/share/mtroamd/mtroamd.sock
Restart=on-failure
RestartSec=5
# KillMode=process so ` + "`systemctl restart`" + ` only SIGTERMs the main
# daemon — the per-session pty-sidecar children survive in their
# cgroup waiting for the new daemon to dial them back. The
# default (control-group) wipes every sidecar + child shell on
# unit cycle, defeating v0.6.0's restart-resilient PTY split.
KillMode=process
# Cgroup ceilings. Everything the daemon owns shares this cgroup -
# the daemon, every session's pty-sidecar, their child shells, and
# any agent (Claude/codex/agy) a user runs inside one. Uncapped,
# a runaway or a leak walks the whole box into swap thrash and the
# machine stops responding WITHOUT the OOM killer ever firing (box
# freeze 2026-08-06: 5.0G resident + 4.0G swap on a 7.7G host,
# oom_kill count 0). The v1.7.3 per-sidecar oom_score_adj only picks
# WHO dies once the kernel decides someone must; it cannot stop the
# thrash. These do:
#   MemoryHigh    soft throttle - reclaim pressure starts here
#   MemoryMax     hard cap - the in-cgroup OOM killer takes ONE
#                 process (a session), never the whole box
#   MemorySwapMax the load-bearing one - a small swap budget is what
#                 forces a prompt in-cgroup kill instead of hours of
#                 thrashing. It takes no percentage, so it is absolute.
# The percentages are of installed RAM, so these are sane on a 2G VPS
# and a 64G workstation alike. Override with a drop-in
# (~/.config/systemd/user/mtroamd.service.d/*.conf) - drop-ins survive
# ` + "`mtroamd migrate`" + ` and reinstall, edits to this file do not.
MemoryHigh=40%
MemoryMax=55%
MemorySwapMax=512M

[Install]
WantedBy=default.target
`
	got := RenderUserUnit(nil)
	if got != want {
		t.Errorf("default unit output drifted; diff:\n--- want ---\n%s--- got ---\n%s", want, got)
	}
}

// TestRenderUserUnitContainsMemoryCeilings pins the cgroup caps independently of
// the byte-exact golden, so a reflow of the comment block cannot quietly drop
// them. Without these the daemon's cgroup is unbounded and a leak or a runaway
// session takes the whole machine down by thrashing swap - which is what
// happened on 2026-08-06, with oom_kill still reading 0 afterwards.
func TestRenderUserUnitContainsMemoryCeilings(t *testing.T) {
	got := RenderUserUnit(nil)
	for _, want := range []string{
		"\nMemoryHigh=40%\n",
		"\nMemoryMax=55%\n",
		"\nMemorySwapMax=512M\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted unit is missing %q — the cgroup would be uncapped", strings.TrimSpace(want))
		}
	}
}

func TestRenderUserUnitContainsKillModeProcess(t *testing.T) {
	got := RenderUserUnit(nil)
	if !strings.Contains(got, "\nKillMode=process\n") {
		t.Error("emitted unit is missing KillMode=process — sidecars would not survive restart")
	}
}

func TestRenderUserUnitHonoursOverrides(t *testing.T) {
	got := RenderUserUnit(&UserUnitOptions{
		BinPath:    "/opt/mtroamd/bin/mtroamd",
		Addr:       "100.64.0.1:51820",
		SocketPath: "/run/mtroamd/mtroamd.sock",
		TCPAddr:    "100.64.0.1:51821",
	})
	wantExec := "ExecStart=/opt/mtroamd/bin/mtroamd serve --addr 100.64.0.1:51820 --mtroam-tcp-addr 100.64.0.1:51821 --socket /run/mtroamd/mtroamd.sock"
	if !strings.Contains(got, wantExec) {
		t.Errorf("override ExecStart line missing; got:\n%s", got)
	}
}

// TestRenderUserUnitTCPOptOut verifies the sentinel "-" suppresses
// the --mtroam-tcp-addr flag entirely. Used by operators who want
// QUIC-only on hosts where the TCP port can't be reserved.
func TestRenderUserUnitTCPOptOut(t *testing.T) {
	got := RenderUserUnit(&UserUnitOptions{TCPAddr: "-"})
	if strings.Contains(got, "--mtroam-tcp-addr") {
		t.Errorf("TCPAddr=\"-\" should suppress the --mtroam-tcp-addr flag; got:\n%s", got)
	}
}

// TestFlakeModulesCarryMemoryCeilings guards the two hand-maintained Nix copies
// of the unit against drifting away from RenderUserUnit.
//
// The unit text now lives in four places: this package (the SSOT), the NixOS
// module and the home-manager module in flake.nix, and a Swift literal in the
// iOS repo used for the copy-paste recovery script. Only the SSOT is covered by
// the golden above, and a silently-uncapped cgroup is exactly the condition that
// froze a box on 2026-08-06 — so the two copies that live in THIS repo get a
// cheap grep-level check. (The Swift copy is out of reach from Go; it is called
// out in the doc comment on RenderUserUnit.)
func TestFlakeModulesCarryMemoryCeilings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "flake.nix"))
	if err != nil {
		t.Skipf("flake.nix not readable from here: %v", err)
	}
	flake := string(raw)
	for _, want := range []string{`MemoryHigh = "40%";`, `MemoryMax = "55%";`, `MemorySwapMax = "512M";`} {
		// Two modules (NixOS + home-manager), so each directive appears twice.
		if got := strings.Count(flake, want); got != 2 {
			t.Errorf("flake.nix has %d copies of %q, want 2 (NixOS + home-manager modules) "+
				"— a Nix install would get an uncapped cgroup", got, want)
		}
	}
}
