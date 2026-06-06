# AUR packaging

Two flavours, both maintained from this directory:

- **`mtroamd`** — build-from-source via the system Go toolchain. Conventional
  AUR "real package" tier. Build deps: `go>=1.26`, `git`, `pandoc`.
- **`mtroamd-bin`** — pre-built binaries from the GitHub Release.
  Faster install, no Go toolchain required.

The two packages `provides=` and `conflicts=` each other, so users pick one.

## Release flow

**Use the workflow** — after the release workflow for a stable tag finishes:

```sh
gh workflow run publish-aur -f tag=vX.Y.Z -f dry_run=true   # rehearse
gh workflow run publish-aur -f tag=vX.Y.Z -f dry_run=false  # publish
```

`publish-aur.yml` does the whole prep in an archlinux container against the
live release assets (pkgver bump → minisign-verify SHA256SUMS + binary
cross-check → regenerate `sha256sums_*` arrays via `makepkg -g` → regenerate
both `.SRCINFO`s → makepkg smoke-test both packages → push both AUR repos).
Prep-at-publish-time means the in-tree PKGBUILDs are just templates and can't
go stale against pruned releases.

The dev box cannot publish directly: aur.archlinux.org is SSH-22-only and
this box's egress is tailscale-locked — the `AUR_SSH_PRIVATE_KEY` secret in
CI is the only push path.

### Manual flow (what the workflow automates)

1. `make aur-prep VERSION=vX.Y.Z` (repo root) — rewrites `pkgver` in both
   `PKGBUILD`s. **That is all it does** — sums and `.SRCINFO` are separate
   steps. The source `mtroamd` PKGBUILD uses `sha256sums=('SKIP')` for its
   git-tag source — integrity is pinned via the upstream tag.
2. Regenerate the `sha256sums_*` arrays in `mtroamd-bin/PKGBUILD` from the
   published assets: `CARCH=<arch> makepkg -g` per arch.
3. Regenerate `.SRCINFO` in each package directory:
   ```sh
   cd packaging/aur/mtroamd && makepkg --printsrcinfo > .SRCINFO
   cd packaging/aur/mtroamd-bin && makepkg --printsrcinfo > .SRCINFO
   ```
4. Smoke-test locally: `cd packaging/aur/mtroamd-bin && makepkg -si --noconfirm`.
   Then `mtroamd version` should report the tagged version.
5. Push each PKGBUILD + .SRCINFO to its AUR repo at
   `ssh://aur@aur.archlinux.org/<pkgname>.git` (branch `master`). Each AUR
   package is its own git repo on the AUR remote.

## Why two packages instead of one with a `--source` flag

AUR convention. `foo-bin` (binary) and `foo` (source) are the two standard
shapes and users expect to find both. The `provides=`/`conflicts=` pair
prevents accidental dual-install.

## Long game: AUR promotion to `extra`

Promotion needs ~10 AUR votes or 1% pkgstats usage, plus three Package
Maintainers agreeing and one volunteering to maintain. Year-out goal, not a
now goal — keep both PKGBUILDs working and let the votes accumulate.
