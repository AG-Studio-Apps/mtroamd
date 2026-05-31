# mtroamd v0.3.0 — what's new

## `mtroam` ships in the release artifacts

The laptop CLI has existed in this repo for a while but wasn't part
of the published release. v0.3.0 changes that: every signed release
now includes `mtroam-*` binaries for the same seven platforms as
`mtroamd-*`, all covered by the single `SHA256SUMS.minisig`.

Install: download → verify signature → drop in `~/.local/bin/mtroam`.
Full guide in `docs/mtroam.md`.

This unblocks attaching to a remote mtRoam session from your laptop's
terminal — same persistent-shell experience the iOS app gives you,
but on a real keyboard.

## `mtroam update` and `mtroam uninstall`

Mirroring `mtroamd update` / `mtroamd uninstall`:

- `mtroam update` — checks GitHub Releases, verifies the minisign
  signature on the new SHA256SUMS, verifies the binary's SHA-256,
  atomically swaps `~/.local/bin/mtroam` in place. Anti-rollback is
  on by default — `--allow-downgrade` to override.
- `mtroam uninstall` — removes the binary. No state directory to
  worry about; mtroam has no cert/key/socket of its own.

## Shared internal/release package

The semver comparison helpers (`ParseSemver`, `CompareSemver`,
`VersionsMatch`, `BaseTag`) moved from `cmd/mtroamd/update.go` to
`internal/release/version.go` so both binaries' update flows share
one implementation. No user-visible behaviour change.

## Compatibility

- `mtroamd` and `mtroam` should be paired at the same version or
  newer-`mtroam` / older-`mtroamd`. Wire protocol is unchanged in
  this release.
- iOS app version unchanged. The next iOS build will start dropping
  `mtroam` alongside `mtroamd` on auto-installed hosts, so anyone
  who SSHes into a mtRoam host has `mtroam` ready to go.
