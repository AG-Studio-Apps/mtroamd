# Cross-Front Security Scan — 2026-06-04

Full-fronts scan of the mtroamd + meshTerm stack: static analysis, dependency
CVEs, binary scan of the shipped v1.5.1 artifacts, secret scanning across both
repos' full history, GitHub Actions workflow audit, and an end-to-end review of
the Daemon Channel trust chain and the daemon's network attack surface.

Run by: on-demand (not the 06:00 vuln-watch CI / 06:33 autonomous routine).
Scope: mtroamd `develop` @ `ccb3679`, meshTerm `develop` @ `d5411e1`, released
binaries `v1.5.1`, published `channel-ios` manifest (serial 3 / pin v1.5.1).

## TL;DR

**No critical, high, or exploitable findings. Nothing requires rotation or an
emergency release.** One actionable dependency bump (a non-reachable medium
CVE), plus a handful of low-severity hardening notes.

| Front | Tool / method | Result |
|---|---|---|
| Go CVEs (reachable) | govulncheck source + binary | **Clean** — 0 vulnerabilities |
| Go CVEs (by version) | osv-scanner, Dependabot | 1 medium, **not reachable** (quic-go) |
| Static analysis | gosec full, staticcheck | high/high gate **clean (0)**; rest benign |
| Secrets | gitleaks full history, both repos | **No real leaks** — 27 hits all benign |
| Binary | govulncheck -mode=binary, checksums, minisign | **Clean** + signatures verify |
| Channel integrity | minisign verify + serial/pin reconcile | **Verified** |
| Trust chain (manual) | adversarial review, both repos | **Sound** — no bypass found |
| Network surface (manual) | adversarial review, Go daemon | **No High/Med** |
| Workflows | zizmor, actionlint | 3rd-party SHA-pinned; low hardening notes |
| Module integrity | go mod verify | all modules verified |

## Actionable

### A1 — Bump quic-go 0.59.0 → 0.59.1 (Medium CVE, NOT reachable)
- **GHSA-vvgj-x9jq-8cj9** / Dependabot alert #3, CVSS 5.3: HTTP/3 QPACK trailer
  expansion memory exhaustion. Flagged by osv-scanner and Dependabot (by
  version).
- **Reachability: none.** `govulncheck` in both source and binary mode reports
  *no vulnerabilities* — mtroamd uses raw QUIC, not HTTP/3 QPACK, so the
  vulnerable code path is not in the call graph. The shipped v1.5.1 binary is
  not exposed.
- **Action:** bump anyway to clear the Dependabot alert and stay current. Will
  require re-locking `flake.nix` vendorHash (CI prints the correct hash). This
  is the finding the 06:33 routine *should* pick up — it is a good live test of
  whether the routine acts on a Dependabot-only / osv signal vs. govulncheck
  (which is silent here, since the path is unreachable).
- **Resolution (live test result):** both the 06:00 vuln-watch CI and the 06:33
  autonomous routine ran clean and took no action — confirming both are
  govulncheck-only and blind to this by-version finding. Bumped to v0.59.1 in
  this same release (v1.5.2). Follow-up: add Dependabot-alert ingestion as a
  detect-and-notify path (issue + assign), *not* the auto-fix-and-release path,
  since by-version findings are not reachability-gated.

## Hardening (low severity, no urgency)

### H1 — Daemon Channel: no key revocation (Med, by design)
A compromised **primary** minisign key stays a trusted verifier in every fielded
app build until an App Store update ships a new key — the channel cannot revoke
itself. Mitigated by offline custody of the signing keys and the emergency-key
rotation path. *Option:* add a signed `min-key-serial` / revocation field to the
manifest schema (the reserved `min` slot is a natural home) so the emergency key
can mark the primary untrusted fleet-wide without Apple. Track for v2.1.

### H2 — Daemon Channel: anti-replay serial resets on uninstall (Low)
Last-seen serial lives in UserDefaults, so reinstall lets an old *validly-signed*
manifest replay. **Bounded harmless** by the client-enforced compiled floor: a
replayed manifest can never select a pin below the freshly-installed build's own
pin, so it cannot downgrade the daemon. *Option:* persist the serial in the
Keychain (survives reinstall).

### H3 — Plaintext-TCP bind allows `0.0.0.0`/`::` with only a warning (Low)
`internal/transport/tailnet.go:126-135` (`guardPlaintextBind`) refuses concrete
globally-routable addresses but lets an **unspecified** bind through with a
`slog.Warn`. Operator-gated and non-default (the iOS-facing listener is
hard-bound `127.0.0.1:0`), but `--mtroam-tcp-addr 0.0.0.0:N` would expose the
un-TLS'd session protocol on every interface. *Fix:* fail closed on unspecified
binds unless an explicit `--allow-public-plaintext` opt-in is passed, or
post-bind enumerate interfaces and refuse if any concrete address is globally
routable.

### H4 — SID-mismatch attach branch doesn't strike the rate limiter (Informational)
`internal/transport/protocol_handler.go:521-528` returns `unknown_session`
without calling `RecordBadToken`, unlike sibling bad-token branches. Reaching it
requires already consuming a valid single-use token, so it is not weaponizable
for probing — add the strike for consistency only.

### H5 — First-party `actions/*` pinned to major tags, not SHAs (Low, policy)
zizmor flags `actions/checkout@v6`, `actions/setup-go@v6`,
`actions/upload-artifact@v7`, `actions/download-artifact@v8` as unpinned across
ci.yml / release.yml / vuln-watch.yml. **The real supply-chain risk is already
closed:** the two *third-party* actions are SHA-pinned —
`softprops/action-gh-release@b430933…` and
`DeterminateSystems/nix-installer-action@ef8a148…`. First-party `actions/*` on
major tags is a defensible trust-GitHub's-org tradeoff. *Option:* SHA-pin
`actions/*` too for defense-in-depth; not a vulnerability.

### H6 — meshTerm iOS build.yml: ASC secrets referenced outside a dedicated environment (Low)
zizmor `secrets-outside-env` on `ASC_KEY_ID` / `ASC_ISSUER_ID` /
`ASC_PRIVATE_KEY` (build.yml:365-367). Hardening only — move the signing job to
a protected GitHub Environment so the App Store Connect credentials are scoped +
gated. No leak.

### H7 — gitleaks noise: add an allowlist (Informational)
The 27 secret-scanner hits (all benign — see below) will retrip every scan.
Add a `.gitleaksignore` for `meshTermTests/SSHKeyParserTests.swift` and the
`*InstallConstants.swift` `*KeyBase64` lines so future scans stay signal-rich.

## Front-by-front detail

### Static analysis (mtroamd)
- **gosec**: 78 total issues, but the **high-severity / high-confidence gate
  (the permanent CI bar, excluding G115/G702/G703) is clean — 0 issues.** The 78
  are medium/low: G301/G302/G306 file-permission notes on *local install* paths
  (systemd unit files at 0644 — correct, units must be world-readable; bin dirs
  at 0755 — standard) and a G104 unhandled `os.Stdout.Sync()`. None are
  network-reachable; the actual key material is written 0600 in a uid-verified
  0700 dir (confirmed in the network review).
- **staticcheck**: code-quality only — unused funcs (U1000), error-string style
  (ST1005), and a loop-condition smell in `cmd/mtroam/predict.go:412/426`
  (SA4008/SA4004) worth a cosmetic look. No security impact.

### Dependencies
- govulncheck (develop + main, source): **No vulnerabilities found.**
- govulncheck `-mode=binary` on v1.5.1 `mtroamd-linux-amd64` + `mtroam-linux-amd64`:
  **No vulnerabilities found.**
- osv-scanner / Dependabot: the single quic-go finding (A1).
- `go mod verify`: all modules verified.

### Secrets (both repos, full history + working tree)
gitleaks: mtroamd 265 commits → 1 hit; meshTerm 825 commits → 15 hits + 12 in
working tree. **All 27 verified benign** by manual review:
- **3 public minisign keys** — `primaryKeyBase64` / `emergencyKeyBase64` in
  `*InstallConstants.swift` + `internal/release/roster.go` (and a pre-rebrand
  copy in history). RWS/RWR-prefixed 42-byte Ed25519 *public* verification keys;
  embedding is intentional. Private counterparts were never committed.
- **10 test fixtures** — `SSHKeyParserTests.swift` throwaway keys
  (`ssh-keygen`-generated, `meshterm-test-*` comments, encrypted ones use
  passphrase `testpass`), never used against real hosts.
- **14 false positives** — Swift type names (`Curve25519.Signing.PrivateKey`),
  PEM armor markers in code/comments, and user-guide prose.

**No real leaked private keys or credentials. No rotation required.**

### Binary scan (v1.5.1 release artifacts)
- `sha256sum -c SHA256SUMS`: all match.
- `minisign -V` of SHA256SUMS against the embedded primary key: **verified**
  (trusted comment `mtroamd v1.5.1`).
- govulncheck binary mode: clean (above).

### Channel integrity (Daemon Channel)
- Fetched `channel-ios` release assets: `ios-channel.json` =
  `{"schema":1,"serial":3,"pin":"v1.5.1"}`.
- `minisign -V` against embedded primary key: **verified**, trusted comment
  `mtroamd ios-channel serial 3 pin v1.5.1` — serial + pin reconcile with the
  JSON body.

### Trust chain (manual, adversarial — both repos)
Reviewed signature-before-trust, authenticated-trusted-comment-bound-to-content,
monotonic-serial replay protection, client-enforced same-MAJOR.MINOR
non-decreasing downgrade floor, and fail-closed fallback to the compiled pin.
Go and Swift verifiers are in lockstep; BLAKE2b-512 + Ed25519 implemented
correctly for both `Ed`/`ED` minisign algorithms. Two-layer design holds: the
channel only *selects* a tag; the selected release is independently
re-authenticated by SHA256SUMS + tag-binding at install and again by the
daemon's verified `update --tag`. **No forge / replay / downgrade bypass found.**
Residual hardening: H1, H2. Doc drift (informational): prose says `channel/ios`,
shipped code uses `channel-ios`.

### Network attack surface (manual, adversarial — Go daemon)
Auth: 128-bit crypto/rand single-use attach tokens, 30s TTL, constant-time
compare, per-source accept rate-limit + bad-token cooldown shared across QUIC +
TCP. Authz: a token authorizes exactly one token-bound session; no
client-SID-driven lookup. Exec: `exec.Command` direct-execve, env allowlist, no
network input reaches argv/env — `Exec`/`Shell` arrive only over the
SSH-authenticated IPC boundary, not the network. Frame/CBOR parsing is
allocation-bounded; sessions + attachers capped; TLS 1.3, P-256, tickets
disabled, fingerprint-pinned. **No High/Med findings.** Low: H3, H4.

## Tooling
govulncheck, gosec, staticcheck (preinstalled); gitleaks, osv-scanner,
actionlint, zizmor 1.25.2 (installed for this run). Manual review via subagents
for the trust-chain, network-surface, and secret-verification passes.

## Comparison to 2026-06-01 (v1.4.11 hardening release)
That scan hardened the daemon pre-merge (SourceLimiter, bind guard, FS_NODUMP
key backup-exclude, env-file fail-closed, x/net bump, gosec CI gate). This scan
confirms those hold and extends coverage to the **new** Daemon Channel surface
(shipped 2026-06-03) end-to-end, plus the first binary + cross-repo secret
sweep. Net new actionable since then: A1 (quic-go bump).
