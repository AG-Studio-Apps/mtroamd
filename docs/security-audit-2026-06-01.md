# mtroamd security audit — 2026-06-01

Folded into the v1.4.9 packaging release (auto-setup on package install). Triggered
by two GitHub CodeQL alerts that the web UI gated behind paid Advanced Security;
both were retrieved via the API and resolved, then a deeper review was run over the
network-facing daemon surfaces and the (newly root-running) install scriptlets.

Scope: QUIC/TLS transport (server + client), local IPC socket + session authz,
shell/process spawning, and the nfpm root-context scriptlets. Three independent
review passes; findings reconciled below.

## CodeQL alerts (the "flagged, can't view" items)

| # | Rule | Where | Disposition |
|---|------|-------|-------------|
| 5 | `go/disabled-certificate-check` (HIGH) | `cmd/mtroam/attach_quic.go:46` | **Dismissed — false positive.** `InsecureSkipVerify` is compensated by a constant-time SHA-256 fingerprint pin in `VerifyPeerCertificate`; TLS 1.3 min; resumption-bypass closed on both ends (client has no `ClientSessionCache`, server sets `SessionTicketsDisabled:true`, `server.go:156`), so the pin always runs. |
| 4 | `actions/missing-workflow-permissions` (MEDIUM) | `.github/workflows/macos-install-smoke.yml` | **Fixed** — added top-level `permissions: contents: read` (workflow only fetches public release assets). Closes on next scan. |

## Architecture (confirmed sound)

Authentication is **SSH + a single-use attach token**, not the TLS cert. A client
SSHes in, runs `mtroamd connect`, which mints a 128-bit CSPRNG token (single-use,
30s TTL, constant-time compare, atomic consume) over the authenticated SSH channel
and returns the server cert fingerprint. The QUIC/TCP peer then proves possession of
that token. The cert pin is a MITM gate, not the authz gate. Verified: a non-paired
**local** user cannot reach the IPC socket (0600 in a uid-verified 0700 dir,
symlink-refusing), and a **remote** peer cannot open a PTY without a live token. No
command/argument-injection path — all shells spawn via `exec.Command(argv...)`, never
`sh -c`, and argv/cwd/env are never peer-controlled (curated env allowlist).

Cert generation is exemplary: P-256, `crypto/rand`, 0600 key, atomic write, state-dir
ownership+mode audit that fails closed.

## Fixed in v1.4.9

1. **Postremove privilege drop (MEDIUM)** — `packaging/nfpm/postremove.sh` +
   `rpm-postremove.sh` did `grep`/`rm -f`/`rm -rf` **as root** on paths under the
   user's own `$HOME` (`~/.config/.../mtroamd.service`, `~/.local/share/mtroamd`) —
   a symlink/TOCTOU footgun (a planted symlink could steer a root delete). Now run
   via `run_user` as `$SUDO_USER`, with the path passed as an argv arg (no
   interpolation) and a `! -L` guard on the state dir. Dropped to the user, a planted
   symlink can only delete that user's own files.
2. **Misleading transport comment (INFO)** — `internal/transport/server.go` claimed
   "bind to localhost only," but production supervisors bind `0.0.0.0:49820`. Comment
   corrected to describe the real model (network-reachable by design; token-gated).
3. **`EnableDatagrams` (LOW)** — flipped `true → false`; no protocol path uses QUIC
   datagrams, so it was unused attack surface on a network-reachable listener.
4. **Scriptlet comment accuracy (INFO)** — the new bus-wait loop's "POSIX sh"
   comment over-claimed (`sleep 0.2` is a coreutils extension); reworded.

## Backlog (documented, not blocking v1.4.9)

- **Public-listener DoS hardening (MEDIUM if bound to `0.0.0.0`)** — concurrency is
  capped (64-slot semaphore) but there is no per-source accept rate-limit or
  bad-token attempt throttle. A real peer can churn TLS handshakes + token scans.
  Suggest a token-bucket on accepts and/or a bad-token cooldown. Largely a non-issue
  for loopback/tailnet deployments; matters only for genuine `0.0.0.0` exposure.
- **Plaintext TCP bind not enforced to tailnet (MEDIUM)** — `tcp_server.go` runs the
  same handler over un-TLS'd TCP, safe only if bound to a tailnet/loopback address.
  The `tailnet:` sentinel does this, but an explicit `--mtroam-tcp-addr 0.0.0.0:...`
  is allowed silently. Suggest refusing / loudly warning on a globally-routable bind.
- **TLS private key backup exposure (LOW)** — `~/.local/share/mtroamd/key.pem` is
  swept into ordinary home backups (no nodump/backup-exclude, unlike the iOS tsnet
  state). Impact bounded (compromise only enables MITM of the still-token-gated QUIC
  channel). Document as sensitive; consider a backup-exclude hint.
- **`loadEnvFile` empty-path fallback (LOW)** — `internal/ptysidecar/sidecar.go`
  returns the full `os.Environ()` when launched without `--env-file`. Unreachable in
  the normal daemon flow (an env-file is always written from the curated allowlist),
  but a future caller omitting the flag would leak the daemon's full env to the
  child. Add a guard/comment.
