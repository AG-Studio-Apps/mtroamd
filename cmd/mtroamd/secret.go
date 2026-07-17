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

	if *session == "" || *file == "" {
		fmt.Fprintln(os.Stderr, "mtroamd set-secrets: --session and --file are required")
		return connectExitGenericError
	}

	data, readErr := os.ReadFile(*file)
	_ = os.Remove(*file) // always delete a staged secret file
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
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "mtroamd secret-exec: usage: secret-exec <command> [-- args...]")
		return connectExitGenericError
	}
	command := args[0]
	cmdArgs := args[1:]
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}

	shimDir := os.Getenv("MESHTERM_SHIM_DIR")
	realPath, err := secret.ResolveReal(command, shimDir, os.Getenv("PATH"))
	if err != nil {
		// Can't even find the real command - surface it (this is not a
		// broker failure; the command genuinely isn't on PATH).
		fmt.Fprintf(os.Stderr, "mtroamd secret-exec: %v\n", err)
		return connectExitGenericError
	}

	// Best-effort secret fetch. Every failure path here FALLS OPEN to a
	// plain exec of the real command.
	if sid := os.Getenv("MESHTERM_SESSION_ID"); sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resp, gerr := ipc.NewClient(discoverClientSocketPath(), time.Second).
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
