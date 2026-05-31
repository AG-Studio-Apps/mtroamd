package svcmgr

import (
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

[Install]
WantedBy=default.target
`
	got := RenderUserUnit(nil)
	if got != want {
		t.Errorf("default unit output drifted; diff:\n--- want ---\n%s--- got ---\n%s", want, got)
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
