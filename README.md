# mtroamd

**mtroamd** is a persistent terminal daemon over QUIC. It holds shell sessions on a host across network drops, device sleep, and client reconnects — like `mosh` + `tmux` consolidated into one daemon, with real scrollback through disconnects, named multi-session, and any-client handoff between devices.

Ships with [`mtroam`](docs/mtroam.md), a laptop CLI for `attach` / `list` / `new` / `kill` / `rename` / `update`. The iOS app [meshTerm](https://meshterm.app) is one client; mtroam is another; the wire protocol is documented in [`docs/mtroam-protocol.md`](docs/mtroam-protocol.md) so others can be written.

Start a build on your phone in the morning, reattach from a laptop in the afternoon. Lose Wi-Fi mid-shell, walk to a café, reconnect — the session is still there with full scrollback.

## Status

Stable. mtroamd is well past v1.0 (current: v1.7.x) and ships in lockstep with the meshTerm iOS app. The wire protocol ([`docs/mtroam-protocol.md`](docs/mtroam-protocol.md)) is the frozen v1 contract within the `meshterm/0` ALPN epoch — additive changes only (unknown message types and fields are ignored, never repurposed). Bug reports against tagged releases are welcome and get triaged.

## Compared to

|                              | mtroamd       | mosh        | tmux        | wezterm mux | Eternal Terminal |
|------------------------------|-----------------|-------------|-------------|-------------|------------------|
| Persistent across drops      | ✅              | ✅          | ✅          | ✅          | ✅               |
| Real scrollback through drop | ✅              | ❌          | ✅          | ✅          | ✅               |
| Named multi-session per host | ✅              | ❌          | ✅          | ✅          | partial          |
| Modern UDP transport         | QUIC            | SSP/UDP     | n/a         | TCP/SSH     | TCP              |
| Mobile-native client         | ✅ (meshTerm)†  | partial     | ❌          | ❌          | ❌               |
| Same daemon, multiple clients| ✅              | ❌          | ✅          | client-specific | ✅           |
| Tab completion / man pages   | ✅              | ✅          | ✅          | ✅          | ✅               |
| Signed self-update           | ✅ (minisign)   | distro-only | distro-only | distro-only | distro-only      |

The daemon is the source of truth; the clients are interchangeable. That's the line wezterm's multiplexer can't easily cross — it requires their emulator on both ends.

† **meshTerm iOS status**: the QUIC-speaking meshTerm app is live on the App Store. The app and daemon ship in lockstep — mtroamd cuts coordinated public releases alongside meshTerm app updates.

## What it does

- Listens for QUIC connections from any client that speaks the mtRoam protocol (ALPN `meshterm/0`, single bidi stream with tagged framing).
- Owns a registry of terminal sessions: PTY + child shell + monotonic output ring buffer (4 MiB per session).
- Sessions persist across client disconnects; reattach replays buffered output from the client's last ack sequence.
- One exclusive + N readonly attachers per session. Multi-attach is for "watch a colleague" / "open the same session from a second device."
- Cert-pinning trust bootstrapped over SSH — no PSK, no custom crypto in the daemon. If you can `ssh user@host`, you have full control over your daemon.
- Self-update with minisign-signed `SHA256SUMS` plus an embedded primary + emergency public-key roster.

## Install

**Prebuilt binaries** from [GitHub Releases](https://github.com/AG-Studio-Apps/mtroamd/releases/latest) for seven targets — linux amd64/arm64/armv7, darwin amd64/arm64, freebsd amd64/arm64. Releases include the daemon, the `mtroam` CLI, man pages, and shell completions for bash/zsh/fish.

```sh
# Pick the right asset for your platform from the latest release.
# Verify SHA-256 against the signed SHA256SUMS, then install:
install -m 755 mtroamd-<platform> ~/.local/bin/mtroamd
install -m 755 mtroam-<platform>     ~/.local/bin/mtroam
```

The minisign public key for `SHA256SUMS.minisig` verification lives in [`docs/release-public-key.txt`](docs/release-public-key.txt).

**Package managers** — signed repositories on the same release line as the App Store app:

- **Debian / Ubuntu (apt)** — a signed apt repo on the same release line as the App Store app:

  ```sh
  curl -fsSL https://ag-studio-apps.github.io/mtroamd/mtroamd-archive-keyring.gpg \
    | sudo tee /usr/share/keyrings/mtroamd-archive-keyring.gpg > /dev/null
  echo "deb [signed-by=/usr/share/keyrings/mtroamd-archive-keyring.gpg] https://ag-studio-apps.github.io/mtroamd stable main" \
    | sudo tee /etc/apt/sources.list.d/mtroamd.list
  sudo apt update && sudo apt install mtroamd
  # That's it — the package enables + starts the daemon for your login user and
  # turns on linger so it survives logout + reboot. Check with: mtroamd doctor
  #
  # Installed as root / non-interactively (no $SUDO_USER)? Finish as your user:
  #   sudo loginctl enable-linger "$USER" && systemctl --user enable --now mtroamd
  ```

  Or grab a `.deb` straight from a release and `sudo dpkg -i mtroamd_*_<arch>.deb` (verify against `SHA256SUMS-deb`). A pre-release **development** channel exists for testers — unstable, see [`docs/apt-dev-channel.md`](docs/apt-dev-channel.md).

  The unit ships cgroup ceilings (`MemoryHigh=40%`, `MemoryMax=55%`, `MemorySwapMax=512M`, percentages of installed RAM) so a runaway session cannot take the host down. Tune them with a drop-in at `~/.config/systemd/user/mtroamd.service.d/*.conf` — drop-ins survive upgrades and `mtroamd migrate`; edits to the unit file itself do not.

  Uninstall: `sudo apt remove mtroamd` stops + removes the daemon but keeps your sessions/cert (so a reinstall reuses the same identity); `sudo apt purge mtroamd` removes those too for a full clean wipe.
- **Fedora (dnf)** — a GPG-signed yum/dnf repo (`x86_64`, `aarch64`):

  ```sh
  sudo tee /etc/yum.repos.d/mtroamd.repo <<'EOF'
  [mtroamd]
  name=mtroamd
  baseurl=https://ag-studio-apps.github.io/mtroamd/rpm/stable/$basearch
  enabled=1
  gpgcheck=1
  gpgkey=https://ag-studio-apps.github.io/mtroamd/mtroamd-archive-keyring.asc
  EOF
  sudo dnf install mtroamd
  # That's it — the package enables + starts the daemon for your login user and
  # turns on linger so it survives logout + reboot. Check with: mtroamd doctor
  #
  # Installed as root / non-interactively (no $SUDO_USER)? Finish as your user:
  #   sudo loginctl enable-linger "$USER" && systemctl --user enable --now mtroamd
  ```

  A pre-release **development** channel exists for testers — unstable, see [`docs/dnf-dev-channel.md`](docs/dnf-dev-channel.md). Uninstall: `sudo dnf remove mtroamd`.
- **Homebrew tap** (macOS, Linux): `brew tap AG-Studio-Apps/mtroamd && brew install mtroamd`
- **Arch Linux (AUR)**: `mtroamd-bin` (pre-built) and `mtroamd` (build-from-source)
- **NixOS / Nix** — a flake ships the package + a NixOS module and a home-manager
  module (declarative `systemd --user` service + linger):

  ```nix
  # flake.nix
  inputs.mtroamd.url = "github:AG-Studio-Apps/mtroamd";
  # NixOS:        imports = [ mtroamd.nixosModules.default ];
  #              services.mtroamd = { enable = true; users = [ "you" ]; };
  # home-manager: imports = [ mtroamd.homeManagerModules.default ];
  #              services.mtroamd.enable = true;   # set users.<you>.linger at system level
  ```

  Or just try it: `nix run github:AG-Studio-Apps/mtroamd -- version`.
- **Alpine (apk)** — a signed apk repo; Alpine has no systemd, so it ships an
  **OpenRC** service. See [`docs/apk-dev-channel.md`](docs/apk-dev-channel.md):

  ```sh
  curl -fsSL https://ag-studio-apps.github.io/mtroamd/mtroamd-apk.rsa.pub \
    | sudo tee /etc/apk/keys/mtroamd-apk.rsa.pub > /dev/null
  echo "https://ag-studio-apps.github.io/mtroamd/apk/stable" \
    | sudo tee -a /etc/apk/repositories
  sudo apk add mtroamd
  # set MTROAMD_USER in /etc/conf.d/mtroamd (the daemon spawns shells as it), then:
  sudo rc-update add mtroamd && sudo rc-service mtroamd start
  ```

The Homebrew formula, `PKGBUILD`s, and apt/rpm/apk packaging (`nfpm` config + repo setup) live under [`packaging/`](packaging/); the signed channels are published with each release.

Once installed, the daemon usually runs under a supervisor — systemd-user on Linux, launchd on macOS, or a `nohup` fallback. The supervisor unit is dropped automatically by the iOS app's auto-installer on first connect, by the distro packages on `pacman -S` / `brew install`, or by hand:

```
mtroamd unit print > ~/.config/systemd/user/mtroamd.service
systemctl --user daemon-reload
systemctl --user enable --now mtroamd
```

`mtroamd unit print` is the single source of truth for the systemd-user unit — it's what every install path emits, so the `KillMode=process` setting (load-bearing for v0.6+ pty-sidecar restart-resilience) is always in place.

## Companion CLI: `mtroam`

`mtroam` is the laptop/desktop counterpart to the iOS app — manages remote sessions over SSH and attaches to them as your local terminal. Same binary distribution; same release artifacts.

Full install + usage guide: [`docs/mtroam.md`](docs/mtroam.md). Man page: `man mtroam`.

```
mtroam --host me@dev-box list                       # what's alive on the daemon
mtroam --host me@dev-box new --name dev             # create without attaching
mtroam --host me@dev-box attach dev                 # land in the same shell
                                                   # your iPhone is using
mtroam --host me@dev-box attach dev --mode readonly # watch over someone's
                                                   # shoulder; can't type
mtroam --host me@dev-box rename dev staging
mtroam --host me@dev-box kill staging
mtroam --host me@dev-box status                     # daemon snapshot
```

In an attached session, type `~.` on a fresh line to detach (mosh / ssh convention). The remote shell stays alive on the daemon; pick it up from any other client.

Set `MTROAM_HOST` once per shell or write the target into `~/.config/mtroam/host` to omit `--host`.

Auth is plain SSH — we shell out to the system `ssh` binary, so your existing `~/.ssh/config`, ssh-agent, and keys work transparently. The QUIC connection that carries the attached terminal is cert-pinned to the fingerprint received over SSH (same trust hop the iOS app uses).

Transport-layer security is TLS 1.3 (provided by Go's standard library inside `quic-go`); we add cert pinning bootstrapped over SSH. There is no application-layer cryptography in this codebase.

## Reporting issues

Bugs and questions about the daemon, `mtroam`, or the wire protocol: file an issue on this repo. Templates are provided.

Bugs about the **meshTerm iOS app** (UI, host management, anything that isn't the daemon): use the in-app help/feedback channel — the meshTerm app source is private, so issues here aren't the right venue for app-side problems.

Feature requests are triaged; since the wire protocol is stable (v1), additive changes are preferred. Bug reports against released versions are always welcome.

## Reporting security issues

See [SECURITY.md](docs/SECURITY.md). **Do not file security reports as public issues.**

## License

GNU Affero General Public License v3.0 or later (AGPL-3.0-or-later) — see
[LICENSE](LICENSE). Copyright © 2026 AG Studio Apps.

A commercial / proprietary license (for use without the AGPL's network-copyleft
obligations) is available from AG Studio Apps — contact the maintainer.
