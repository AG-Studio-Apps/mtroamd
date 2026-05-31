package main

import (
	"strings"
	"testing"

	"github.com/AG-Studio-Apps/meshtermd/internal/svcmgr"
)

func TestParseExecStartFlags(t *testing.T) {
	cases := []struct {
		name                        string
		unit                        string
		wantAddr, wantSock, wantTCP string
	}{
		{
			name: "full default unit (addr+tcp+socket)",
			unit: "[Service]\nExecStart=%h/.local/bin/meshtermd serve --addr 0.0.0.0:49820 " +
				"--roam-tcp-addr tailnet:49920 --socket %h/.local/share/meshtermd/meshtermd.sock\n",
			wantAddr: "0.0.0.0:49820",
			wantSock: "%h/.local/share/meshtermd/meshtermd.sock",
			wantTCP:  "tailnet:49920",
		},
		{
			name:     "QUIC-only unit (no --roam-tcp-addr) → preserved as sentinel",
			unit:     "[Service]\nExecStart=%h/.local/bin/meshtermd serve --addr 1.2.3.4:5000 --socket /tmp/m.sock\n",
			wantAddr: "1.2.3.4:5000",
			wantSock: "/tmp/m.sock",
			wantTCP:  "-",
		},
		{
			name:     "custom addr is preserved",
			unit:     "ExecStart=%h/.local/bin/meshtermd serve --addr 100.64.0.1:49820 --roam-tcp-addr 100.64.0.1:49920 --socket /run/x.sock",
			wantAddr: "100.64.0.1:49820",
			wantSock: "/run/x.sock",
			wantTCP:  "100.64.0.1:49920",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := svcmgr.UserUnitOptions{}
			parseExecStartFlags(tc.unit, &opts)
			if opts.Addr != tc.wantAddr {
				t.Errorf("Addr = %q, want %q", opts.Addr, tc.wantAddr)
			}
			if opts.SocketPath != tc.wantSock {
				t.Errorf("SocketPath = %q, want %q", opts.SocketPath, tc.wantSock)
			}
			if opts.TCPAddr != tc.wantTCP {
				t.Errorf("TCPAddr = %q, want %q", opts.TCPAddr, tc.wantTCP)
			}
		})
	}
}

// A round-trip guard: flags parsed out of a rendered default unit, fed back
// into RenderUserUnit with a new BinPath, must reproduce the same bind config
// (only the binary path changes) — this is the core migration invariant.
func TestMigrateRoundTripPreservesBindConfig(t *testing.T) {
	orig := svcmgr.RenderUserUnit(&svcmgr.UserUnitOptions{BinPath: "%h/.local/bin/meshtermd"})
	opts := svcmgr.UserUnitOptions{BinPath: "/usr/bin/meshtermd"}
	parseExecStartFlags(orig, &opts)
	got := svcmgr.RenderUserUnit(&opts)

	if want := "ExecStart=/usr/bin/meshtermd serve"; !strings.Contains(got, want) {
		t.Errorf("rewritten unit missing %q\n---\n%s", want, got)
	}
	for _, flag := range []string{"--addr 0.0.0.0:49820", "--roam-tcp-addr tailnet:49920",
		"--socket %h/.local/share/meshtermd/meshtermd.sock"} {
		if !strings.Contains(got, flag) {
			t.Errorf("rewritten unit dropped preserved flag %q\n---\n%s", flag, got)
		}
	}
}
