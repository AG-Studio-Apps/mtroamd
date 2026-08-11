package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/ptysidecar"
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
	// Every allocate reports the daemon-instance boot id (16 hex chars):
	// iOS keys its broker "already delivered" cache on it, so an absent
	// or per-allocate-varying id would force (or wrongly skip) re-pushes.
	if len(resp.BootID) != 16 {
		t.Errorf("allocate BootID = %q, want 16 hex chars", resp.BootID)
	}
	// ★★ ShimReady is FALSE here, and that is the corrected behaviour, not a
	// regression. This helper allocates `/bin/sh -c "while true; …"`: POSIX sh
	// with non-benign args, so the sidecar seeds NO rcfile and nothing defines
	// _mt_shim_path. shimSpawnEnv putting the shim dir first at spawn is not
	// the same guarantee: the flag now means "we seeded this shell, so the
	// re-assert runs every prompt", because the case that matters is
	// `bash -c "tmux new"`, where every pane reads the user's untouched rc and
	// an ordinary `PATH="$HOME/bin:$PATH"` outranks the shim. Reporting true
	// there let iOS call a secret push `.delivered` while the tool launched
	// with no secrets.
	//
	// ★ Accepted cost: a NON-interactive `-c` session (this one) runs no rc at
	// all, so its shim really does stay first and the warning is spurious. The
	// daemon cannot tell that apart from the tmux case without guessing at the
	// command, and this file's stated bias is that a false "ready" suppressing
	// a needed warning is the worse error.
	if resp.ShimReady == nil || *resp.ShimReady {
		t.Errorf("unseeded `sh -c` ShimReady = %v, want *false", resp.ShimReady)
	}
	return resp.SessionID
}

// TestAllocateShimReadyIsLiveFromDisk pins the POSITIVE end-to-end wiring:
// shim-ready file → readShimStatus → AllocateResponse.ShimReady.
//
// ★★ This is the assertion the review found missing. Before it, the only test in
// the tree touching ShimReady asserted it was *false, so the entire chain could
// have been dead - a renamed status file, a path typo, a dropped field - and the
// suite would still have passed green on both CI legs. A barrier that is only
// ever tested in its "closed" state is not tested.
//
// It also pins the LIVENESS that the whole fix depends on. The shell announces
// readiness at its FIRST PROMPT, which is necessarily after the spawn returned,
// so a daemon that snapshots the bit at spawn (as every version before rc3 did)
// can only ever report "not yet". Writing the file after the session exists and
// then reattaching reproduces exactly that ordering.
func TestAllocateShimReadyIsLiveFromDisk(t *testing.T) {
	t.Parallel()
	d, c, cleanup := startDaemon(t)
	defer cleanup()
	ctx := context.Background()

	// allocSession asserts the freshly spawned, unseeded session reports *false.
	sid := allocSession(t, c)

	// Stand in for `_mt_shim_announce` running at the shell's first prompt: it
	// echoes THIS spawn's nonce, which the sidecar published at seed.
	sessDir := filepath.Join(d.stateDir, "sessions", sid)
	statusPath := filepath.Join(sessDir, ptysidecar.ShimStatusFilename)
	noncePath := filepath.Join(sessDir, ptysidecar.ShimNonceFilename)
	nonce, err := os.ReadFile(noncePath)
	if err != nil {
		t.Fatalf("sidecar published no shim nonce: %v", err)
	}
	if len(nonce) == 0 {
		t.Fatal("shim nonce is empty")
	}
	if err := os.WriteFile(statusPath, nonce, 0o600); err != nil {
		t.Fatalf("write shim status: %v", err)
	}

	resp, err2 := c.Allocate(ctx, ipc.AllocateRequest{SessionID: sid, Rows: 24, Cols: 80})
	if err2 != nil || !resp.Ok {
		t.Fatalf("reattach allocate: %v %s %s", err2, resp.Err, resp.Msg)
	}
	if resp.ShimReady == nil || !*resp.ShimReady {
		t.Fatalf("after announce, ShimReady = %v, want *true (the daemon must re-read the file, not reuse its spawn-time snapshot)", resp.ShimReady)
	}

	// And it must track the file DOWNWARD too, so a respawn that resets the bit
	// cannot leave a stale "ready" behind.
	if err := os.WriteFile(statusPath, []byte("0"), 0o600); err != nil {
		t.Fatalf("reset shim status: %v", err)
	}
	resp, err2 = c.Allocate(ctx, ipc.AllocateRequest{SessionID: sid, Rows: 24, Cols: 80})
	if err2 != nil || !resp.Ok {
		t.Fatalf("second reattach allocate: %v %s %s", err2, resp.Err, resp.Msg)
	}
	if resp.ShimReady == nil || *resp.ShimReady {
		t.Errorf("after reset, ShimReady = %v, want *false", resp.ShimReady)
	}

	// ★★ A LITERAL "1" must NOT be accepted. That is exactly what v1.7.8-v1.7.10
	// sidecars wrote, under much weaker rules, and rc3 read those files verbatim -
	// so the upgrade hole the release set out to close stayed open through the
	// status file. Only this spawn's nonce counts.
	if err := os.WriteFile(statusPath, []byte("1"), 0o600); err != nil {
		t.Fatalf("write legacy status: %v", err)
	}
	resp, err2 = c.Allocate(ctx, ipc.AllocateRequest{SessionID: sid, Rows: 24, Cols: 80})
	if err2 != nil || !resp.Ok {
		t.Fatalf("legacy-status allocate: %v %s %s", err2, resp.Err, resp.Msg)
	}
	if resp.ShimReady != nil && *resp.ShimReady {
		t.Error("a legacy literal \"1\" was accepted as ready; pre-v1.7.11 status files must not be trusted")
	}
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

	// Duplicate keys are rejected at THIS trust boundary too, not only in
	// ParsePayload - a direct IPC caller must not get silent last-wins.
	r3, err := c.SetSessionSecrets(ctx, ipc.SetSessionSecretsRequest{
		SessionID: sid,
		Secrets: []ipc.SecretEntry{
			{Key: "A", Value: "1", Cmds: []string{"gh"}},
			{Key: "A", Value: "2", Cmds: []string{"git"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r3.Ok || r3.Err != ipc.ErrBadRequest {
		t.Errorf("dup key: Ok=%v Err=%q, want bad_request", r3.Ok, r3.Err)
	}
}
