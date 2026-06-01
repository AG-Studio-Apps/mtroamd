# mtroamd security & vulnerability scan — 2026-06-01

A tooling-driven static + binary scan run before merging `develop → main`,
complementing the same-day manual/CodeQL review in
[`security-audit-2026-06-01.md`](./security-audit-2026-06-01.md). That audit
reasoned about the design (TLS pinning, authz, process spawn, root scriptlets);
this pass adds the automated layer — known-CVE reachability, SAST, and binary
hardening — and remediates everything surfaced.

Toolchain: Go 1.26.3 · govulncheck v1.3.0 · gosec · staticcheck 2026.1.

## Method

Three phases, as planned:

1. **Static** — `go vet`, `gosec`, `staticcheck`, `govulncheck` (source/reachability mode).
2. **Build** — reproducible release build (`CGO_ENABLED=0 -tags netgo -trimpath -ldflags="-s -w -buildid="`).
3. **Binary** — `govulncheck -mode=binary` (authoritative on linked code), build-hardening
   inspection, embedded-secret sniff.

**Scope note:** the `develop→main` delta is **100 % packaging** (NixOS flake, Alpine
apk, CI workflows, docs) — zero Go source changes. The Go findings below are therefore
pre-existing (identical on `main`), assessed here because the user gated the `main`
release on a clean scan, not because this merge introduces them.

## Results

### govulncheck — clean
- Source mode: **0 reachable** vulnerabilities.
- Binary mode (pre-fix): 6 advisories in `golang.org/x/net@v0.54.0` (`html.Parse`),
  reported as linked though source-mode found them unreachable. **Fixed** by bumping to
  `v0.55.0` (indirect dep); both modes now report 0.

### gosec — 79 raw issues, 0 real
All HIGH findings triaged to false positives, each verified in code:

| Rule | Count | Where | Disposition |
|------|-------|-------|-------------|
| G402 / G123 (TLS) | 2 | `cmd/mtroam/attach_quic.go` | FP. `InsecureSkipVerify` gated by a constant-time SHA-256 fingerprint pin in `VerifyPeerCertificate`; no `ClientSessionCache` + server `SessionTicketsDisabled` ⇒ resumption can't skip the pin. Annotated `#nosec G402,G123` with rationale so a *new* occurrence still fails the gate. |
| G115 (int overflow) | ~35 | wire/protocol/ringbuf/buffer | FP. Every conversion is encode-side `uint32(len(x))` behind a `> Max…` check, or decode-side validated (`n > MaxControlFrameBytes`) *before* `make()`. All MEDIUM-confidence. |
| G703 (path traversal, taint) | 8 | `svcmgr/nohup.go`, `pty.go`, `lifecycle.go`, `migrate.go` | FP. Taint sources are the daemon's own env (`XDG_RUNTIME_DIR`, `os.UserHomeDir()`), never a peer. Experimental taint rule, high FP. |
| G702 (cmd injection, taint) | 1 | `ptysidecar/sidecar.go:88` | FP. `exec.Command(argv...)`, never `sh -c`; argv/env never peer-controlled. Experimental taint rule. |
| G204 / G304 / G301 / G306 … | 33 | various | MEDIUM/LOW; standard gosec mechanical noise (e.g. a 0644 systemd unit file). |

### staticcheck — non-security
10 findings, all style / dead-code (unused funcs, error-string casing, a `predict.go`
loop lint). No security relevance.

### Binary hardening — strong
`trimpath` ✓ · `CGO_ENABLED=0` (no libc surface) ✓ · stripped ✓ · non-exec stack (NX) ✓
· clean VCS build (`vcs.modified=false`) ✓ · no embedded secrets ✓.
Minor: `buildmode=exe` (not PIE) — Go default; full-ASLR would need `-buildmode=pie`. Left as-is.

### Packaging / CI (the actual merge content)
- Secret handling in `release.yml` is correct: `ABUILD_KEY` consumed via env
  (`printf '%s' "$ABUILD_KEY"`), never `${{ }}`-interpolated into a `run:` block;
  fork-gated. Workflow permissions minimally scoped (`contents: read` / `write`). Key-gen
  perms 0600/0700.
- **Fixed:** `DeterminateSystems/nix-installer-action@main` (mutable branch) and
  `softprops/action-gh-release@v3` pinned to commit SHAs.

## Remediations applied (all before merge)

| ID | Item | Sev | Fix |
|----|------|-----|-----|
| B1 | `x/net` advisories | LOW | Bumped `golang.org/x/net` → v0.55.0. |
| P1 | Unpinned CI actions | LOW-MED | SHA-pinned `nix-installer-action` (`ef8a148`, v22) + `action-gh-release` (`b430933`, v3). |
| #4 | `loadEnvFile` env leak | LOW | Empty env-file path now fails closed; inheritance gated behind a test-only `AllowInheritedEnv`. |
| #2 | Plaintext-TCP bind | MED | `guardPlaintextBind` refuses a globally-routable bind for the un-TLS'd TCP listener; warns on unspecified; allows loopback/private/link-local/tailnet. |
| #3 | Key backup exposure | LOW | Best-effort `FS_NODUMP` on `key.pem` (Linux; no-op elsewhere) + SECURITY.md checklist line. |
| #1 | No accept throttle | MED | `SourceLimiter`: per-source token bucket + bad-token cooldown, shared across QUIC + TCP + handler; bounded swept map. From-scratch (no new deps). Tested with an injected clock. |

**Known limitation (#1):** the limiter keys on source IP, not IPv6 /64 — fine for the
loopback/tailnet/LAN deployments mtroamd targets; /64-aggregation for a genuinely public
IPv6 bind is a documented future refinement.

## Permanent CI gate (added to `.github/workflows/ci.yml`)
- **govulncheck** (`./...`): hard-fails on any finding.
- **gosec** `--severity high --confidence high --exclude G702,G703,G115`: the experimental
  taint rules (G702/G703) and the mechanical MEDIUM-confidence integer-conversion rule
  (G115) are excluded as audited-here false positives; G402/G123 remain gated and are
  individually `#nosec`-justified, so any *new* high-severity/high-confidence finding fails CI.
- Tool versions pinned for reproducibility.

## Verification
- `govulncheck ./...` and `govulncheck -mode=binary` → 0.
- `gosec --severity high --confidence high --exclude G702,G703,G115 ./...` → exit 0.
- `go vet ./...` clean; `go test ./...` green (incl. new `ratelimit_test.go`,
  `TestGuardPlaintextBind`, the `AllowInheritedEnv` sidecar tests).
