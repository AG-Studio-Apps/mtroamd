# meshtermd dnf — development channel (Fedora)

> ⚠️ **DEVELOPMENT CHANNEL — UNSTABLE. MAY BREAK YOUR SYSTEM.**
> The `dev` channel carries pre-release builds from `develop` (versioned
> `…~rc`/`…~dev`). It pairs with the **TestFlight** meshTerm app, not the App
> Store release, and can ship protocol changes ahead of the stable daemon. Use
> it only for testing on a disposable host. **For normal use, follow the stable
> install in the [README](../README.md).**

## Opt in

```sh
sudo tee /etc/yum.repos.d/meshtermd-dev.repo <<'EOF'
[meshtermd-dev]
name=meshtermd (development — unstable)
baseurl=https://ag-studio-apps.github.io/meshtermd/rpm/dev/$basearch
enabled=1
gpgcheck=1
gpgkey=https://ag-studio-apps.github.io/meshtermd/meshtermd-archive-keyring.asc
EOF

sudo dnf install meshtermd
# then, as your login user:
systemctl --user enable --now meshtermd
```

Every dev install/upgrade prints a `⚠ DEVELOPMENT build` warning, and
`rpm -q meshtermd` / `dnf info meshtermd` show the `~rc`/`~dev` version.

## Go back to stable

The dev version sorts *below* the matching stable release, so drop the dev repo
and let dnf converge:

```sh
sudo rm -f /etc/yum.repos.d/meshtermd-dev.repo
# add the stable repo from the README, then:
sudo dnf distro-sync meshtermd
```

(If you're on a `~rc` newer than the latest stable, `dnf` won't downgrade
automatically — `sudo dnf downgrade meshtermd` once stable has a release.)

## Uninstall

```sh
sudo dnf remove meshtermd
```

`%postun` stops + disables your `--user` service and removes the unit on final
removal; your state in `~/.local/share/meshtermd` (cert, sessions) is kept.
Other users on the box manage their own service.

## Note on signatures

Packages are signed with an **ed25519** GPG key — verified fine by modern Fedora
(rpm 4.18+ / F38+). Older RPM stacks (RHEL 8/9) may not verify ed25519 package
signatures; this channel targets Fedora.
