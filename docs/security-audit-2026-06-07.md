# mtroamd security audit — 2026-06-07

Pre-dev-release gate audit (mtroamd installer-coexistence cycle). Three
independent passes: (1) filesystem / install / uninstall paths, (2) network
listeners + auth + rate-limiting, (3) process exec / pty-sidecar / secrets /
deps. Method: manual code review (skeptical, exploit-scenario driven). The
automated `govulncheck` + `gosec` gates run in CI (no Go toolchain on the audit
box; CI is the binding check for those).

## Verdict

**No Critical, High, or exploitable findings. Cleared for the dev-channel
release.** The cycle's new change (`mtroamd uninstall` now removes the sibling
`mtroam` CLI) is **safe**. Residual items are all Low/Info — pre-existing,
documented, and accepted under the single-user trust model.

## Cycle change reviewed: uninstall sibling-removal (`cmd/mtroamd/uninstall.go`)

`mtroamPath := filepath.Join(filepath.Dir(binPath), "mtroam"); os.Remove(...)`
after the binary removal. **Safe:**
- **No path traversal** — `binPath = release.JoinBin()` is the hardcoded
  `$HOME/.local/bin/mtroamd`; never network/client/env-influenced.
- **No symlink abuse** — `os.Remove` does not follow the final path component, so
  a pre-planted `~/.local/bin/mtroam → /etc/x` symlink only unlinks the symlink.
- **Package-managed guard is load-bearing and strictly precedes removal** — the
  `release.IsPackageManaged()` early-return (exit 2) guarantees `binPath` is the
  user-local copy before any `os.Remove`, so a root-owned `/usr/bin` path can
  never be reached. (Asserted in a code comment.)
- **TOCTOU between `fileExists` and `os.Remove` is benign** — uninstall is
  idempotent; a vanished file is logged, not fatal.

## Findings

| # | Area | Severity | Status |
|---|------|----------|--------|
| 1 | uninstall sibling-removal (this cycle) | Info | Safe — see above |
| 2 | Plaintext TCP bind to `0.0.0.0` warns but doesn't forbid | Low/Info | Operator responsibility; opt-in flag, `guardPlaintextBind` blocks globally-routable IPs, attach still needs a token over SSH |
| 3 | Per-IP (not per-/64) rate-limiting | Info | Documented future hardening for public-IPv6; defaults bind loopback/tailnet |
| 4 | Session names visible to IPC socket readers | Info | Socket is 0600 + parent-dir uid/≤0700 verified; reader already OS-trusted |
| 5 | fg-detection reads `/proc/<pgid>/comm`+`cmdline` | Low | Same-uid only (proc perms); reads process *name*/argv[0], not full args; sidecar already runs as the user |
| 6 | Atomic-update temp file relies on `os.CreateTemp` default 0600 | Info | Correct on POSIX; optional explicit chmod for clarity |

None block release. (2)–(6) are unchanged from prior audits and accepted.

## Well-guarded (verified this pass)

- **Auth:** 16-byte `crypto/rand` attach tokens, 30s TTL, single-use,
  `subtle.ConstantTimeCompare` on token *and* session-id; validated pre-protocol.
- **Rate-limiting:** `SourceLimiter` (token bucket + bad-token cooldown) enforced
  on every accept *before* handshake; inflight semaphore (64) sheds load;
  pre-auth read deadlines (5s) defeat slowloris.
- **Transport:** TLS 1.3 min, `SessionTicketsDisabled` (no 0-RTT pinning bypass),
  `NoClientCert` (iOS pins fingerprint), `MaxIncomingStreams:1`/uni:0, no
  datagrams (no amplification), 1200B initial packet.
- **CBOR:** strict mode (map≤64, array≤256, nesting≤8); 64KiB control / 16KiB
  output frame caps; overflow-safe length handling; PTY dims clamped 3–1000.
- **Filesystem:** cert/key written atomically at 0600 + FS_NODUMP; state dir +
  IPC socket parent verified uid-owned ≤0700; IPC socket symlink-refusing, 0600;
  log opened `O_NOFOLLOW`.
- **Exec/env:** argv-only (no shell strings); `validateSSHHost` rejects `-…` +
  POSIX `shellQuote`; env-file **fails closed** then unlinks; child shell gets a
  9-var allowlist, never the daemon's full environ.
- **Secrets/logging:** private key never logged; tokens only on the bootstrap
  stdout line (by design, over SSH); no session content logged.
- **Lifecycle:** `killOrphanDaemon` excludes own pid + exact-match pgrep.
- **Error messages:** fixed strings, never echo peer-supplied bytes.

## Dependencies

`go 1.26.3` (toolchain `go1.26.4`, patching GO-2026-5039 / GO-2026-5037);
quic-go v0.59.1, x/crypto v0.52.0, x/net v0.55.0, fxamacker/cbor v2.9.2,
creack/pty v1.1.24, x/sys v0.45.0, x/term v0.43.0. No known-vulnerable versions
identified by manual review; **CI `govulncheck`/`gosec` are the binding gate** —
release on green.

## Sign-off

Cleared for the dev-channel release once CI `govulncheck` + `gosec` pass. No
mandatory pre-release fixes. Optional follow-ups (non-blocking): per-/64 IPv6
rate-limit aggregation; explicit temp-file chmod in `release/atomic.go`.
