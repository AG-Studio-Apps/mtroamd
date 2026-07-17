package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// allocSession spawns a fresh session and returns its id, for the secret
// handler tests.
func allocSession(t *testing.T, c *ipc.Client) string {
	t.Helper()
	resp, err := c.Allocate(context.Background(), ipc.AllocateRequest{
		SessionID: "new", Rows: 24, Cols: 80,
		Shell: "/bin/sh", Exec: []string{"-c", "while true; do sleep 1; done"},
	})
	if err != nil || !resp.Ok {
		t.Fatalf("allocate: %v %s %s", err, resp.Err, resp.Msg)
	}
	return resp.SessionID
}

func TestSecretBrokerHandlersEndToEnd(t *testing.T) {
	t.Parallel()
	d, c, cleanup := startDaemon(t)
	defer cleanup()
	sid := allocSession(t, c)
	ctx := context.Background()

	// Push: GITHUB_TOKEN → gh/git, AWS_KEY → aws.
	set, err := c.SetSessionSecrets(ctx, ipc.SetSessionSecretsRequest{
		SessionID: sid,
		Secrets: []ipc.SecretEntry{
			{Key: "GITHUB_TOKEN", Value: "ghp_v1", Cmds: []string{"gh", "git"}},
			{Key: "AWS_KEY", Value: "akia", Cmds: []string{"aws"}},
		},
	})
	if err != nil || !set.Ok {
		t.Fatalf("set-secrets: %v %s %s", err, set.Err, set.Msg)
	}

	// GetSecrets is least-privilege: gh gets only GITHUB_TOKEN.
	get, err := c.GetSecrets(ctx, ipc.GetSecretsRequest{SessionID: sid, Command: "gh"})
	if err != nil || !get.Ok {
		t.Fatalf("get-secrets gh: %v", err)
	}
	if get.Env["GITHUB_TOKEN"] != "ghp_v1" || len(get.Env) != 1 {
		t.Errorf("gh env = %v, want only GITHUB_TOKEN=ghp_v1", get.Env)
	}
	// An undeclared command gets nothing.
	if u, _ := c.GetSecrets(ctx, ipc.GetSecretsRequest{SessionID: sid, Command: "curl"}); len(u.Env) != 0 {
		t.Errorf("curl env = %v, want empty", u.Env)
	}

	// Shims were generated for the declared commands.
	shimDir := d.shimDirFor(sid)
	for _, cmd := range []string{"gh", "git", "aws"} {
		if _, err := os.Stat(filepath.Join(shimDir, cmd)); err != nil {
			t.Errorf("expected shim for %s: %v", cmd, err)
		}
	}

	// Rotation: a new push replaces the set (GITHUB_TOKEN only) and prunes
	// the aws shim; gh sees the new value with no respawn.
	if _, err := c.SetSessionSecrets(ctx, ipc.SetSessionSecretsRequest{
		SessionID: sid,
		Secrets:   []ipc.SecretEntry{{Key: "GITHUB_TOKEN", Value: "ghp_v2", Cmds: []string{"gh"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if g, _ := c.GetSecrets(ctx, ipc.GetSecretsRequest{SessionID: sid, Command: "gh"}); g.Env["GITHUB_TOKEN"] != "ghp_v2" {
		t.Errorf("post-rotation gh = %v, want ghp_v2", g.Env)
	}
	if _, err := os.Stat(filepath.Join(shimDir, "aws")); !os.IsNotExist(err) {
		t.Error("aws shim should have been pruned after the narrowed push")
	}
}

func TestSecretSetRejectsUnknownSessionAndBadInput(t *testing.T) {
	t.Parallel()
	_, c, cleanup := startDaemon(t)
	defer cleanup()
	ctx := context.Background()

	// A well-formed but non-existent session id is rejected (bounds the store).
	bogus := session.SessionID{}.String() // all-zero, 32 hex, not live
	r, err := c.SetSessionSecrets(ctx, ipc.SetSessionSecretsRequest{
		SessionID: bogus,
		Secrets:   []ipc.SecretEntry{{Key: "A", Value: "1", Cmds: []string{"gh"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Ok || r.Err != ipc.ErrUnknownSession {
		t.Errorf("bogus session: Ok=%v Err=%q, want unknown_session", r.Ok, r.Err)
	}

	// An invalid command (trust boundary) is rejected on a live session.
	sid := allocSession(t, c)
	r2, err := c.SetSessionSecrets(ctx, ipc.SetSessionSecretsRequest{
		SessionID: sid,
		Secrets:   []ipc.SecretEntry{{Key: "A", Value: "1", Cmds: []string{"../evil"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Ok {
		t.Error("expected rejection of an invalid command basename")
	}
}
