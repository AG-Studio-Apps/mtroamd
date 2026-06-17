# Security improvements

A running log of security findings that have been **fixed** in `mtroamd`, with the
detection source for each. We publish *fixed* findings only — open or unconfirmed
issues are handled privately under our [security policy](SECURITY.md) (coordinated
disclosure) and are added here once they ship in a release. Entries are listed newest-first.

Detection sources used across the project: `govulncheck`, dependency advisories
(osv-scanner / Dependabot), static analysis (CodeQL / CI-workflow audits), binary
scans, and manual code review (including adversarial multi-agent review).

## v1.7.0 — 2026-06-15

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

## v1.5.2 — 2026-06-04

| Finding | Severity | Source | Fix |
|---|---|---|---|
| quic-go HTTP/3 QPACK trailer memory-exhaustion (GHSA-vvgj-x9jq-8cj9; not reachable in our build) | Medium | dependency advisory | Bumped quic-go 0.59.0 → 0.59.1 |

## v1.4.11 — 2026-06-01 (hardening release)

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

## v1.4.4 — 2026-05-29 (recover hardening)

_Manual code review (static review + follow-up)._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Recover requests could create unbounded detached session work | Medium | manual review | Per-session recovery slot; cancel previous on new start; cap recover grace at 2 minutes |
| Recover cancellation slot could be cleared by an older canceled recovery | Medium | manual review | Generation-token identity guard frees the slot only for the matching generation |

## v1.1.4 — 2026-05-19

_Manual code review (static review)._

| Finding | Severity | Source | Fix |
|---|---|---|---|
| Self-update signatures not bound to the requested release tag | High | manual review | Trusted-comment tag binding enforced in both update paths |
| Client socket discovery could select a spoofed XDG socket | Medium | manual review | Parent-dir + client-socket ownership verification |
| `mtroam` host values could be parsed as local `ssh` options | Medium | manual review | Host-value validation guard + `--` separator in argv |
