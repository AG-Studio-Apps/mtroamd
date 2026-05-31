# AUR packaging

Two flavours, both maintained from this directory:

- **`mtroamd`** — build-from-source via the system Go toolchain. Conventional
  AUR "real package" tier. Build deps: `go>=1.26`, `git`, `pandoc`.
- **`mtroamd-bin`** — pre-built binaries from the GitHub Release.
  Faster install, no Go toolchain required.

The two packages `provides=` and `conflicts=` each other, so users pick one.

## Release flow

After tagging a new upstream release and the GitHub Actions release workflow
finishes:

1. `make aur-prep VERSION=vX.Y.Z` (from the repo root) rewrites `pkgver`
   in both `PKGBUILD`s and downloads the published `SHA256SUMS` to populate
   `sha256sums_*` arrays in `mtroamd-bin/PKGBUILD`. The source `mtroamd`
   PKGBUILD uses `sha256sums=('SKIP')` for its git-tag source — integrity is
   pinned via the upstream tag.
2. Regenerate `.SRCINFO` in each package directory:
   ```sh
   cd packaging/aur/mtroamd && makepkg --printsrcinfo > .SRCINFO
   cd packaging/aur/mtroamd-bin && makepkg --printsrcinfo > .SRCINFO
   ```
3. Smoke-test locally: `cd packaging/aur/mtroamd-bin && makepkg -si --noconfirm`.
   Then `mtroamd version` should report the tagged version.
4. Push each PKGBUILD + .SRCINFO to its AUR repo at
   `ssh://aur@aur.archlinux.org/<pkgname>.git`. Each AUR package is its own
   git repo on the AUR remote.

## Why two packages instead of one with a `--source` flag

AUR convention. `foo-bin` (binary) and `foo` (source) are the two standard
shapes and users expect to find both. The `provides=`/`conflicts=` pair
prevents accidental dual-install.

## Long game: AUR promotion to `extra`

Promotion needs ~10 AUR votes or 1% pkgstats usage, plus three Package
Maintainers agreeing and one volunteering to maintain. Year-out goal, not a
now goal — keep both PKGBUILDs working and let the votes accumulate.
