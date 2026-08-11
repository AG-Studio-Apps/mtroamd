package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/secret"
)

// runSetSecrets implements `mtroamd set-secrets --session <id> --file
// <json>`: read + delete a client-staged SetSessionSecrets JSON file and
// push it to the daemon's broker. The file is ALWAYS deleted (even on a
// parse error) so a staged secret never lingers - same posture as
// `connect --env-file`. Values transit the local IPC socket into daemon
// memory; nothing persists on disk past this call.
func runSetSecrets(args []string) int {
	fs := flag.NewFlagSet("set-secrets", flag.ExitOnError)
	session := fs.String("session", "", "session id (32 hex chars) to set secrets for")
	file := fs.String("file", "", "path to a SetSessionSecrets JSON file (read then DELETED)")
	socket := fs.String("socket", "", "unix socket path (default: $XDG_RUNTIME_DIR/mtroamd.sock)")
	timeout := fs.Duration("timeout", 5*time.Second, "max time to wait for the daemon")
	_ = fs.Parse(args)

	// Delete the staged file BEFORE any validation exit: the always-delete
	// guarantee must hold even when the client botched --session (review
	// finding — the early return left the 0600 plaintext on disk forever).
	var data []byte
	var readErr error
	if *file != "" {
		data, readErr = os.ReadFile(*file)
		_ = os.Remove(*file) // always delete a staged secret file
	}
	if *session == "" || *file == "" {
		fmt.Fprintln(os.Stderr, "mtroamd set-secrets: --session and --file are required")
		return connectExitGenericError
	}
	if readErr != nil {
		fmt.Fprintln(os.Stderr, "mtroamd set-secrets: read file:", readErr)
		return connectExitGenericError
	}
	payload, err := secret.ParsePayload(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mtroamd set-secrets:", err)
		return connectExitGenericError
	}

	req := ipc.SetSessionSecretsRequest{
		SessionID: *session,
		Secrets:   make([]ipc.SecretEntry, 0, len(payload.Secrets)),
	}
	for _, e := range payload.Secrets {
		req.Secrets = append(req.Secrets, ipc.SecretEntry{Key: e.Key, Value: e.Value, Cmds: e.Cmds})
	}

	socketPath := *socket
	if socketPath == "" {
		socketPath = discoverClientSocketPath()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	resp, err := ipc.NewClient(socketPath, *timeout).SetSessionSecrets(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mtroamd set-secrets: %v\n", err)
		return connectExitGenericError
	}
	if !resp.Ok {
		fmt.Fprintf(os.Stderr, "mtroamd set-secrets: %s: %s\n", resp.Err, resp.Msg)
		if resp.Err == ipc.ErrUnknownSession {
			return connectExitUnknownSession
		}
		return connectExitGenericError
	}
	// ★★ Surface the readiness the daemon reports for THIS push. Without these
	// two lines the fields are unreachable: iOS drives this through
	// `mtroamd set-secrets` over SSH exec and reads its stdout, and every earlier
	// attempt at this signal added a field that no client could observe - the
	// rc3 reason field and the rc4 set-secrets fields both shipped dead.
	//
	// This is the ONE place the answer is load-bearing, because it describes the
	// push that just happened: 1 means the secrets will reach their tools, 0
	// means they will not and the reason line says why.
	if resp.ShimReady != nil {
		v := 0
		if *resp.ShimReady {
			v = 1
		}
		fmt.Printf("MTRM_SHIM_READY %d\n", v)
	}
	if resp.ShimNotReadyReason != "" {
		fmt.Printf("MTRM_SHIM_NOT_READY_REASON %s\n", resp.ShimNotReadyReason)
	}
	return connectExitOK
}

// runSecretExec implements the shim target: `mtroamd secret-exec
// <command> [-- args...]`. It fetches the secrets the daemon declares for
// <command> in this session, injects them into the environment, resolves
// the REAL binary (skipping the shim dir), and execs it. The secret value
// never reaches stdout, the agent's own env, or argv - only the exec'd
// tool's environment.
//
// FAIL-OPEN is the invariant: any failure (no session id, daemon
// unreachable, no secrets) falls through to exec-ing the real command
// UNCHANGED, so a broker hiccup never breaks the user's command.
func runSecretExec(args []string) int {
	// Optional --shim-dir <dir> preceding the command: the generated shim
	// passes it EXPLICITLY so we always know which dir to skip even if the
	// MESHTERM_SHIM_DIR env was cleared (which would otherwise loop). Falls
	// back to the env for a hand-run invocation.
	shimDir := os.Getenv("MESHTERM_SHIM_DIR")
	socketPath := ""
	for len(args) >= 2 {
		if args[0] == "--shim-dir" {
			shimDir = args[1]
			args = args[2:]
			continue
		}
		// --socket: the daemon pins its own socket path into the shim at
		// SyncShims time. Discovery here would run under the session's
		// curated env (no XDG_RUNTIME_DIR), missing a daemon bound to
		// $XDG_RUNTIME_DIR/mtroamd.sock and silently failing open.
		if args[0] == "--socket" {
			socketPath = args[1]
			args = args[2:]
			continue
		}
		break
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "mtroamd secret-exec: usage: secret-exec [--shim-dir D] [--socket S] <command> [-- args...]")
		return connectExitGenericError
	}
	command := args[0]
	cmdArgs := args[1:]
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}

	realPath, err := secret.ResolveReal(command, shimDir, os.Getenv("PATH"))
	if err != nil {
		// The command genuinely isn't installed (a declared "Used by"
		// command with no real binary still gets a shim, so the shell
		// found US instead of nothing). Mimic the shell's own report and
		// exit 127 — a broker-flavored error here reads as a broker bug
		// when the actual situation is plain "not installed" (review
		// finding).
		fmt.Fprintf(os.Stderr, "%s: command not found\n", command)
		return 127
	}

	// Best-effort secret fetch. Every failure path here FALLS OPEN to a
	// plain exec of the real command.
	if sid := os.Getenv("MESHTERM_SESSION_ID"); sid != "" {
		// 2s: a same-host socket round-trip is sub-ms in steady state, so
		// this only bites a slow/hung daemon. 1s was too tight (a GC pause
		// falsely timed out and silently fell open → tool ran token-less);
		// 3s made every shimmed command stall that long against a WEDGED
		// daemon. 2s splits it - tolerates a GC hiccup, bounds a hang.
		if socketPath == "" {
			socketPath = discoverClientSocketPath()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, gerr := ipc.NewClient(socketPath, 2*time.Second).
			GetSecrets(ctx, ipc.GetSecretsRequest{SessionID: sid, Command: command})
		cancel()
		if gerr == nil && resp.Ok {
			for k, v := range resp.Env {
				_ = os.Setenv(k, v)
			}
		}
	}

	argv := append([]string{realPath}, cmdArgs...)
	if err := syscall.Exec(realPath, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "mtroamd secret-exec: exec %s: %v\n", realPath, err)
		return connectExitGenericError
	}
	return connectExitOK // unreachable after a successful exec
}
