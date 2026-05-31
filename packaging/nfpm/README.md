# Debian packaging (nfpm)

`.deb` packages for meshtermd, built with [nfpm](https://nfpm.goreleaser.com)
from the prebuilt binaries `make dist` produces — no Debian toolchain needed.
The CI release job builds one `.deb` per arch and (for `main`/App-Store
releases) publishes them to the apt repo on the `gh-pages` branch.

## What it installs

Mirrors the AUR `meshtermd-bin` layout (FHS paths):

| Path | |
|---|---|
| `/usr/bin/meshtermd`, `/usr/bin/mtctl` | binaries (0755) |
| `/usr/lib/systemd/user/meshtermd.service` | systemd **--user** unit template |
| `/usr/share/man/man{8,1}/…` | man pages |
| `/usr/share/{bash-completion,zsh,fish}/…` | shell completions |
| `/usr/share/doc/meshtermd/{copyright,NOTICE,README.md,SECURITY.md}` | docs |

## Per-user service model (why the scripts are minimal)

meshtermd spawns shells **as the login user**, so it runs as a `systemd --user`
service — never a system daemon. The package therefore **never** creates a
system user, installs a system unit, or auto-enables a service. The maintainer
scripts only:

- **`postinstall.sh`** — (a) loudly flag dev-channel builds (`~rc`/`~dev`
  version); (b) if a prior `~/.local/bin` install (from the iOS app /
  `meshtermd update`) is detected for `$SUDO_USER`, run `meshtermd migrate`
  for them when their user bus is reachable, else print the command; (c) on a
  plain upgrade, `try-restart` the installing user's already-running service so
  it picks up the new binary (no-op if not running — never forces it on).
- **`postremove.sh`** — `apt remove` stops + disables the installing user's
  `--user` service and removes a migrate-created `~/.config` unit pointing at
  the package binary, but **keeps** `~/.local/share/meshtermd` (cert, sessions)
  so a reinstall reuses the identity. `apt purge` additionally wipes that state
  dir for a full clean removal. Per-user (`$SUDO_USER`); all best-effort.

A fresh install prints how to opt in: `systemctl --user enable --now meshtermd`.

## Building locally

```sh
make dist                                            # or one arch: make dist-linux-amd64 …
./dist/meshtermd-linux-amd64 unit print --bin=/usr/bin/meshtermd > dist/meshtermd.service

# .deb (Debian/Ubuntu)
for pair in "amd64 amd64" "arm64 arm64" "armv7 armhf"; do
  set -- $pair
  MESHTERMD_GOARCH=$1 MESHTERMD_PKG_ARCH=$2 MESHTERMD_VERSION=2.0.0 \
    envsubst < packaging/nfpm/nfpm.yaml > dist/nfpm.deb.$2.yaml
  nfpm package -f dist/nfpm.deb.$2.yaml -p deb -t dist/
done

# .rpm (Fedora) — same config, rpm arch names + rpm scriptlets (overrides.rpm)
for pair in "amd64 x86_64" "arm64 aarch64"; do
  set -- $pair
  MESHTERMD_GOARCH=$1 MESHTERMD_PKG_ARCH=$2 MESHTERMD_VERSION=2.0.0 \
    envsubst < packaging/nfpm/nfpm.yaml > dist/nfpm.rpm.$2.yaml
  nfpm package -f dist/nfpm.rpm.$2.yaml -p rpm -t dist/
done
```

`envsubst` (from `gettext-base`) renders the env-templated `src`/`arch`/
`version` fields — nfpm doesn't expand env vars inside `contents` globs.
Inspect with `dpkg-deb -c` / `dpkg --info`.
