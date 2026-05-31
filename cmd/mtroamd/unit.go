package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/AG-Studio-Apps/mtroamd/internal/svcmgr"
)

// runUnit dispatches `mtroamd unit <action>` subcommands.
//
// Today the only action is `print`, which writes the canonical
// systemd-user unit to stdout. A future `mtroamd unit install`
// could land the file in `~/.config/systemd/user/` + run
// `systemctl --user daemon-reload`; we keep it as a no-op stub so
// the surface is forward-compatible.
//
// Exit codes:
//
//	0  printed (or installed) cleanly
//	2  bad action / flag usage
func runUnit(args []string) int {
	if len(args) == 0 {
		unitUsage(os.Stderr)
		return 2
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "print":
		return runUnitPrint(rest, os.Stdout)
	case "help", "--help", "-h":
		unitUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "mtroamd unit: unknown action %q\n", action)
		unitUsage(os.Stderr)
		return 2
	}
}

func runUnitPrint(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("unit print", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	binPath := fs.String("bin", "",
		"absolute path baked into ExecStart (default %h/.local/bin/mtroamd)")
	addr := fs.String("addr", "",
		"QUIC bind address host:port (default 0.0.0.0:49820)")
	socket := fs.String("socket", "",
		"IPC socket path (default %h/.local/share/mtroamd/mtroamd.sock)")
	tcpAddr := fs.String("mtroam-tcp-addr", "",
		"MTRoam-over-TCP listener bind address (default tailnet:49920). "+
			"Pass `-` to suppress the listener entirely (QUIC-only).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	content := svcmgr.RenderUserUnit(&svcmgr.UserUnitOptions{
		BinPath:    *binPath,
		Addr:       *addr,
		SocketPath: *socket,
		TCPAddr:    *tcpAddr,
	})
	if _, err := io.WriteString(out, content); err != nil {
		fmt.Fprintf(os.Stderr, "mtroamd unit print: write: %v\n", err)
		return 1
	}
	return 0
}

func unitUsage(w io.Writer) {
	fmt.Fprintf(w, `mtroamd unit — emit / manage the systemd-user unit file

Usage: mtroamd unit <action> [flags]

Actions:
  print    Write the canonical unit file to stdout. Pipe to
           ~/.config/systemd/user/mtroamd.service to install.

print flags:
  --bin=PATH      override ExecStart binary path
  --addr=H:P            override QUIC bind address (default 0.0.0.0:49820)
  --mtroam-tcp-addr=H:P   override MTRoam-over-TCP bind address (default tailnet:49920)
                        Pass '-' to omit the flag (QUIC-only).
  --socket=PATH         override IPC socket path

Example:
  mtroamd unit print > ~/.config/systemd/user/mtroamd.service
  systemctl --user daemon-reload
  systemctl --user enable --now mtroamd
`)
}
