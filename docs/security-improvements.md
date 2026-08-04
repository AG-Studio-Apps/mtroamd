# Security improvements

A running log of security findings that have been **fixed** in `mtroamd`, with the
detection source for each. We publish *fixed* findings only - open or unconfirmed
issues are handled privately under our [security policy](SECURITY.md) (coordinated
disclosure) and are added here once they ship in a release. Entries are listed newest-first.

Detection sources used across the project: `govulncheck`, dependency advisories
(osv-scanner / Dependabot), static analysis (CodeQL / CI-workflow audits), binary
scans, manual code review (including adversarial multi-agent review), and
independent agent audits (e.g. Codex 5.5).

## v1.7.9 (2026-08-04)

_Pre-stable security audit of the `v1.7.8..v1.7.9` diff — a single change area, the **alt-screen grid-persistence reattach**: a full-frame redraw synthesized at attach time from the daemon's own screen model + ring buffer (so a cold-start reattach to a running TUI gets a complete screen, fixing alt-screen spill), plus the `meta.cbor` restore that seeds that model on boot — with a full static / binary / dependency scan; details + pre-stable gate in `.security/security-audit-v1.7.9.md`. **Clean — no findings, nothing to fix.** `go vet`, `govulncheck` (source + freshly rebuilt daemon+cli binaries on go1.26.5), `osv-scanner`, `gosec`, `staticcheck`, and `gitleaks` were all clean or prior-accepted; `go test -race` passed on the three changed packages (altscreen / session / transport). The delta is **terminal rendering/replay logic** and touches **no** security-sensitive surface — `internal/secret`, `internal/ipc`, `internal/daemon`, `internal/release`, `internal/cert`, and the network/QUIC path are byte-unchanged since v1.7.8. The two new untrusted-input touchpoints are both bounds-checked: the `meta.cbor` restore caps a hostile blob (`maxPersistedScreenRepaint` 1 MiB, gating `Screen.Feed` on load as well as save) and geometry (`maxRestoredScreenDim` 1000, before `altscreen.New` allocates a `rows*cols` grid) — crash-loop-DoS hardening for a corrupt same-uid state dir; and the remote attach dims are gated by `dimsInBounds` before any resize, with injected bytes synthesized from the daemon's own model, never from remote input. No Critical / High / Medium / Low. No dependency change (`go.mod`/`go.sum` unchanged). (This grid-persistence work was reverted out of the v1.7.8 stable cut and re-landed here for v1.7.9.)_

## v1.7.8 (2026-08-04)

_Pre-stable security audit of the `v1.7.7..v1.7.8` diff — the headline change is the **secret broker** (deferred from v1.7.6 for its own review): an in-memory, value-hiding secret store held **RAM-only** (never persisted, never in the session snapshot), pushed per-session over the local `0600` IPC socket (`SetSessionSecrets`) and delivered to consuming tools at exec time via PATH-shadow shims + `mtroamd secret-exec`, so the value never lands in the agent's own env, in argv (`ps`), or in stdout — plus the new **doctor `--fix` / stale-daemon `/proc` census** repair path and an `update` that refuses to report success until the live daemon serves the freshly-installed tag — with a full static / binary / dependency scan; details + pre-stable gate in `.security/security-audit-v1.7.8.md`. **Clean — no findings, nothing to fix.** `go vet`, `govulncheck` (source + freshly rebuilt daemon+cli binaries on go1.26.5), `osv-scanner`, `gosec`, `staticcheck`, and `gitleaks` were all clean or prior-accepted; `go test -race` passed on the changed packages (incl. the new `secret` package); and a focused adversarial re-attack of the broker (5 hypotheses — remote/QUIC reach, shim shell-injection, secret-to-disk leak, shim self-exec loop, and cross-uid shim/`SIGTERM` reach) refuted every candidate. The broker's trust boundary is re-enforced at both the `ParsePayload` and daemon-handler sites (POSIX key charset + safe-basename command allowlist + duplicate-key reject); delivery and readout are **same-uid over the daemon's own socket** — the remote path only *attaches* and can never dispatch broker IPC — and the `/proc` census is strictly same-uid-filtered (a `SIGTERM` can only ever reach this user's own stale `mtroamd serve`). No Critical / High / Medium / Low. No dependency change (`go.mod`/`go.sum` unchanged); no network-surface change (`internal/release` / `internal/cert` untouched). (Two Info-level notes only, both same-uid: `GetSecrets` performs no session-ownership check, and `SecretEntry.Value` isn't newline/size-bounded at the merge.)_

## v1.7.7 (2026-07-20)

_Pre-stable security audit of the `v1.7.6..v1.7.7` diff — **live secret injection**: a bash/zsh prompt hook (seeded by the sidecar after the user's rc, via a generated `--rcfile` / `ZDOTDIR`) that sources+removes a `0600` `~/.mt-inject-<sessionID>` file on each prompt to deliver secrets into a **running** shell, plus the additive `MTRM_LIVE_INJECT` reporting line — with a full static / binary / dependency scan; details + pre-stable gate in `.security/security-audit-v1.7.7.md`. **Clean — no findings, nothing to fix.** `go vet`, `govulncheck` (source + freshly rebuilt daemon+cli binaries on go1.26.5), `osv-scanner`, `gosec`, `staticcheck`, and `gitleaks` were all clean or prior-accepted; `go test -race` passed on the five changed packages (incl. the new hook seeder); and a focused adversarial re-attack (5 hypotheses — shell injection into the generated rc, path traversal via an overridable `MESHTERM_SESSION_ID`, cross-uid/remote injection, `--rcfile` arg-rewrite hijack, and TOCTOU/fail-safe) refuted every candidate. The feature is a **same-uid convenience**: the drop file is delivered over the user's own authenticated SSH/SFTP, and sourcing a `$HOME` file grants nothing a same-uid actor already lacks — with correct `shSingleQuote` escaping, a strict `-l`/`-i` arg allowlist, and a fail-safe seeder that never breaks the shell. No Critical / High / Medium / Low. No dependency change (`go.mod`/`go.sum` unchanged). (Two Info-level defense-in-depth notes only, both same-uid: `req.Env` values aren't newline-sanitized, and the hook sources the drop file without a perms check — inheriting the `~/.bashrc` trust model.)_

## v1.7.6 (2026-07-18)

_Pre-stable security audit of the `v1.7.5..v1.7.6` diff (bound the displaced-client `Goodbye` push at 1s; optional additive session-reuse reporting — `AllocateResponse.Reused` / the `MTRM_SESSION_REUSED` bootstrap line) plus a full static / binary / dependency scan; details + pre-stable gate in `.security/security-audit-v1.7.6.md`. **Clean — no findings, nothing to fix.** `go vet`, `govulncheck` (source + freshly rebuilt daemon+cli binaries on go1.26.5), `osv-scanner`, `gosec`, `staticcheck`, and `gitleaks` were all clean or prior-accepted; `go test -race` passed on the changed packages; and a focused adversarial re-attack (4 hypotheses — the bounded-notify goroutine/race/DoS, and whether the reuse bit feeds any authz or leaks cross-session) refuted every candidate. The notify bound is a DoS **reduction** (an unbounded write became ≤1s); the reuse bit is purely informational and feeds no authz decision. No Critical / High / Medium / Low. No dependency change (`go.mod`/`go.sum` unchanged). (The `wip/secret-broker` feature tagged out-of-band as `v1.7.6-rc3` is NOT in this release and will get its own review before it ships.)_

## v1.7.5 (2026-07-16)

_Pre-stable security audit of the `v1.7.4..v1.7.5` diff (the `connect --env-file` session env-threading, auto-start-on-connect, and the systemd user-manager detection fix) plus a full static / binary / dependency scan; details + pre-stable gate in `.security/security-audit-v1.7.5.md`. **No findings — nothing to fix.** `go vet`, `govulncheck` (source + freshly rebuilt binaries on go1.26.5), `osv-scanner`, `gosec`, `staticcheck`, and `gitleaks` were all clean or prior-accepted, and a focused adversarial re-attack of the env-threading path (6 hypotheses) refuted every candidate boundary crossing. No Critical / High / Medium / Low. No dependency change (`go.mod`/`go.sum` unchanged)._

## v1.7.4 (2026-07-09)

_Pre-stable security audit of the `v1.7.3..v1.7.4` diff plus a full static / binary / dependency scan and a deep adversarial multi-agent review; details + pre-stable gate in `.security/security-audit-v1.7.4.md`. No Critical / High / Medium findings. Two Low items fixed below (first shipping in `v1.7.4-rc3`)._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| `crypto/tls` Encrypted Client Hello privacy leak (GO-2026-5856) linked in the binary and call-graph-reachable via the QUIC listener + the release-fetch HTTPS client. ECH is never configured, so there is no runtime exposure, but the advisory is source-reachable | Low | govulncheck (source) / osv-scanner | Bumped the pinned toolchain `go1.26.4` → `go1.26.5`. Also clears GO-2026-4970 (`os.Root` symlink escape) from the binary - itself unreachable (no `os.Root` use). `govulncheck ./...` is clean on go1.26.5. The `go` directive stays at the language minimum so `GOTOOLCHAIN=local` / Nix builds keep working |
| IPC handler read the request frame with no deadline: a same-uid process (e.g. a compromised session) could open `MaxConcurrentIPCHandlers` (32) connections to the `0600` socket and never send a request, pinning every handler goroutine - denying local allocate/list/kill IPC and stalling graceful shutdown (which waits on in-flight handlers). Symmetric write-stall on the response path (a peer that never drains a send-buffer-filling reply) | Low | adversarial multi-agent code review | Bound each one-shot exchange with two independent deadlines - one on the request read, a fresh one on each response write - so a stalled peer's slot is reclaimed (5s default) while a legitimately slow handler keeps its full write budget. Not remotely reachable (socket is uid-`0600`, parent-dir verified); the attacker already shares the daemon's uid |

## v1.7.3 (2026-07-02, hardening)

_Manual audit of the `v1.7.2..v1.7.3-rc1` diff: no findings. One availability-hardening change (details + a pre-stable release gate in `.security/security-audit-v1.7.3.md`)._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| A runaway session could push the daemon's uncapped cgroup over the box's memory and get `mtroamd` (or another session's sidecar) OOM-killed, dropping persistent sessions - or hard-crash a swapless box | Hardening (availability) | box OOM incident 2026-07-02 | Each `pty-sidecar` raises its own `oom_score_adj` (inherited by the child shell + descendants) strictly ABOVE the daemon's, so the kernel OOM-killer sacrifices the runaway **session**, never `mtroamd`. Relative (inherited baseline + 300), not absolute: a systemd user manager defaults its services to a non-zero score (200 on Ubuntu), so an absolute value below that would be backwards - caught by on-box verification before the stable cut (rc1 `+100` -> rc2 relative-raise). Unprivileged (a protective negative adjust would need `CAP_SYS_RESOURCE`); best-effort + Linux-only. |

## v1.7.2 (2026-06-27)

_Independent Codex 5.5 (gpt-5.5) security audit of the `develop` branch (the new extension-seam batch). No Critical or High findings; two Medium hardening items, first shipping in `v1.7.2-rc2`._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Plaintext-TCP listener could bind an unspecified address (`0.0.0.0` / `::` / host-less `:N`), exposing the un-TLS'd protocol and attach tokens on every interface | Medium | Codex 5.5 audit | `guardPlaintextBind` now **fails closed** on unspecified binds (refused unless `MTROAMD_ALLOW_PLAINTEXT_UNSPECIFIED=1`); the `tailnet:<port>` sentinel and loopback/private/tailnet binds still pass, and concrete globally-routable is still refused. Extends the v1.4.11 globally-routable guard. |
| Stream-extension 64 KiB frame contract was documented but not enforced: one oversized frame slipped past the drop-oldest trim, exceeding the 4 MiB log cap and tearing down client attaches at write time | Medium | Codex 5.5 audit | `Session.PublishFrame` now returns an error and **rejects** any frame over `protocol.MaxControlFrameBytes` (never retained). Not reachable in the stock daemon (no extensions registered); hardens the generic seam the agent fork drives. |

## v1.7.0 - 2026-06-15

_Adversarial multi-agent code review of the recover / attach paths._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Recover idle-gate could SIGTERM a silent foreground tool (risking repo corruption) | High | code review | Kill path re-reads the **live** foreground group and refuses SIGTERM unless it still matches the agent |
| Recover/kill could mislabel any foreground process as "the agent" | Medium | code review | Recover refuses unless the foreground is a recoverable agent |
| Recover reachable by any well-formed client (unconditional exclusive displacement) | Medium | code review | `IsCurrentExclusive(gen)` live-gates stdin / Resize / Recover |
| Displaced exclusive client could still inject stdin/Recover during the displacement window | Medium | code review | Live ownership check under the session lock before acting |
| Two rapid Recover frames raced the single PTY-byte-observer slot → premature kill | Medium | code review | Observer set/clear is generation-keyed |
| Stale foreground anchor + torn AttachAck reads (Cwd/Fg/FgSince) | Low | code review | `ForegroundSnapshot()` returns Fg/FgSince/FgSinceSeq/Cwd as one consistent reading |
| `ClientID` / wire strings unbounded | Low | code review | Explicit 128-byte cap on `Attach.ClientID` |
| `ClientID` match was not constant-time | Low | code review | Switched to `subtle.ConstantTimeCompare` |
| Decoded-but-unused `SavePrompt` (latent command-injection footgun) | Low | code review | Documented as reserved / never injected |

## v1.5.2 - 2026-06-04

| Finding | Severity | Source | Fix |
|---|---|---|---|
| quic-go HTTP/3 QPACK trailer memory-exhaustion (GHSA-vvgj-x9jq-8cj9; not reachable in our build) | Medium | dependency advisory | Bumped quic-go 0.59.0 → 0.59.1 |

## v1.4.11 - 2026-06-01 (hardening release)

_Combined manual audit + tooling-driven scan before the develop → main merge._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Plaintext-TCP bind to a globally-routable address | Medium | manual review | `guardPlaintextBind` refuses globally-routable binds (allows loopback / private / link-local / tailnet) |
| No accept / bad-token throttle on the public listener | Medium | manual review | `SourceLimiter`: per-source token bucket + bad-token cooldown, shared across QUIC + TCP |
| Postremove cleanup ran root `rm`/`grep` over user `$HOME` paths (symlink / TOCTOU) | Medium | manual review | Scriptlets drop privilege to `$SUDO_USER`, with unit/state paths passed as argv (never interpolated) |
| Missing workflow permissions on the macOS install-smoke workflow | Medium | static analysis (CodeQL) | Added top-level `permissions: contents: read` |
| `x/net` advisories linked in the binary (`html.Parse`; unreachable) | Low | govulncheck (binary mode) | Bumped `golang.org/x/net` → v0.55.0 |
| Unpinned third-party CI actions | Low–Med | static analysis (workflow audit) | SHA-pinned `nix-installer-action` + `action-gh-release` |
| `loadEnvFile` could leak the daemon's full env to the child on an empty env-file path | Low | manual review | Empty env-file path now fails closed; inheritance gated behind a test-only flag |
| TLS private key swept into ordinary home backups | Low | manual review | Best-effort `FS_NODUMP` on `key.pem` (Linux) + SECURITY.md checklist line |
| `EnableDatagrams` enabled but unused on a network-reachable listener | Low | manual review | Disabled (removed unused attack surface) |

## v1.4.4 - 2026-05-29 (recover hardening)

_Manual code review (static review + follow-up)._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Recover requests could create unbounded detached session work | Medium | manual review | Per-session recovery slot; cancel previous on new start; cap recover grace at 2 minutes |
| Recover cancellation slot could be cleared by an older canceled recovery | Medium | manual review | Generation-token identity guard frees the slot only for the matching generation |

## v1.1.4 - 2026-05-19

_Manual code review (static review)._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Self-update signatures not bound to the requested release tag | High | manual review | Trusted-comment tag binding enforced in both update paths |
| Client socket discovery could select a spoofed XDG socket | Medium | manual review | Parent-dir + client-socket ownership verification |
| `mtroam` host values could be parsed as local `ssh` options | Medium | manual review | Host-value validation guard + `--` separator in argv |
