# Alpine (apk) packaging

A signed apk repo is published to gh-pages by the `apk-repo` job in
`.github/workflows/release.yml`, routed to a `dev` or `stable` suite by the
same main-ancestry rule as the apt/rpm repos. End-user install lives in the
docs (`docs/apk-dev-channel.md` + the landing page).

## What's here

- **`APKBUILD`** — binary repackage of the prebuilt static `dist/` binaries
  (the nfpm analog; no compilation, musl-safe). Driven by env vars set in CI:
  `MTROAMD_VERSION`, `MTROAMD_GOARCH`, `MTROAMD_DIST`, `MTROAMD_LICENSE`, and
  `CARCH`. A from-source recipe would mirror `packaging/aur/mtroamd/PKGBUILD`.
- **`mtroamd.initd`** — OpenRC service. Alpine has no systemd, so mtroamd runs
  as a system service under `MTROAMD_USER` via `supervise-daemon`. Stop signals
  only the daemon pid; the setsid'd pty-sidecar children survive a restart (the
  OpenRC parallel to the systemd unit's `KillMode=process`).
- **`mtroamd.confd`** — `/etc/conf.d/mtroamd` defaults (`MTROAMD_USER`,
  `MTROAMD_ADDR`, `MTROAMD_TCP_ADDR`, `MTROAMD_SOCKET`).

## Signing key (one-time, maintainer)

The repo is signed with an abuild RSA key. Generate it once and add the private
key to the repo as the `ABUILD_KEY` secret; publish the public half:

```sh
abuild-keygen -a -n            # writes ~/.abuild/<email>-<id>.rsa{,.pub}
# private key  → GitHub secret ABUILD_KEY (paste the .rsa contents)
# public key   → committed/published so `apk` can verify (see docs)
```

## Local build (on Alpine)

```sh
export MTROAMD_VERSION=1.4.10 MTROAMD_GOARCH=amd64 CARCH=x86_64
export MTROAMD_DIST="$PWD/../../dist" MTROAMD_LICENSE="$PWD/../../LICENSE"
abuild checksum && abuild -r
```
