package daemon

import (
	"context"
	"log/slog"

	"github.com/AG-Studio-Apps/mtroamd/internal/ipc"
	"github.com/AG-Studio-Apps/mtroamd/internal/session"
)

// AllocateExtension handles an allocate request whose Kind matches Kind(). It is
// registered by an embedding binary via Config.AllocateExtensions; the core
// ships NONE, so a stock terminal daemon registers no extensions and any
// non-empty Kind is rejected (the Kind dispatch is inert).
//
// This is the generic extension seam: the core knows nothing about what an
// extension does. An extension typically builds a stream-backed session (see
// session.NewStreamSession), registers it, and returns it ready for attach — but
// the core neither requires nor inspects that. Cleanup (goroutine teardown,
// external-resource reaping) is the extension's responsibility, wired via
// session.RegisterCloseHook.
type AllocateExtension interface {
	// Kind is the AllocateRequest.Kind this extension claims. Must be unique
	// across registered extensions and non-empty.
	Kind() string
	// Spawn builds + registers a session for req and returns it ready for
	// attach. It owns the session fully (including Registry.Add).
	Spawn(ctx context.Context, env SpawnEnv, req ipc.AllocateRequest) (*session.Session, error)
}

// SpawnEnv hands an AllocateExtension the core capabilities it needs to build a
// session, without exposing Daemon internals. It is intentionally small; grow it
// only as real extensions require (the seam's compatibility surface).
type SpawnEnv struct {
	Registry    *session.Registry // Add / Lookup / LookupByName / Resolve* / IssueAttachToken
	StateDir    string            // persistence root (may be "")
	BufferBytes int               // default per-session ring capacity (Config.SessionBufferBytes)
	Logger      *slog.Logger
}

// spawnEnv snapshots the daemon capabilities an extension may use.
func (d *Daemon) spawnEnv() SpawnEnv {
	return SpawnEnv{
		Registry:    d.registry,
		StateDir:    d.stateDir,
		BufferBytes: d.cfg.SessionBufferBytes,
		Logger:      d.logger,
	}
}
