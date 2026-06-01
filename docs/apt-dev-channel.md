# mtroamd apt — development channel

> ⚠️ **DEVELOPMENT CHANNEL — UNSTABLE. MAY BREAK YOUR SYSTEM.**
> The `dev` suite carries pre-release builds from `develop` (versioned
> `…~rc`/`…~dev`). It pairs with the **TestFlight** meshTerm app, not the App
> Store release, and can ship protocol changes ahead of the stable daemon. Use
> it only for testing on a disposable host. **For normal use, follow the stable
> install in the [README](../README.md).**

## Opt in

```sh
curl -fsSL https://ag-studio-apps.github.io/mtroamd/mtroamd-archive-keyring.gpg \
  | sudo tee /usr/share/keyrings/mtroamd-archive-keyring.gpg > /dev/null

echo "deb [signed-by=/usr/share/keyrings/mtroamd-archive-keyring.gpg] https://ag-studio-apps.github.io/mtroamd dev main" \
  | sudo tee /etc/apt/sources.list.d/mtroamd-dev.list

sudo apt update && sudo apt install mtroamd
# That's it — the package enables + starts the daemon for your login user and
# turns on linger so it survives logout + reboot. Check with: mtroamd doctor
#
# Installed as root / non-interactively (no $SUDO_USER)? Finish as your user:
#   sudo loginctl enable-linger "$USER" && systemctl --user enable --now mtroamd
```

Every dev install/upgrade prints a `⚠ DEVELOPMENT build` warning, and
`mtroamd --version` / `apt policy mtroamd` show the `~rc`/`~dev` suffix.

## Uninstall

```sh
sudo apt remove mtroamd     # stops + removes the daemon; keeps ~/.local/share/mtroamd (cert, sessions)
sudo apt purge  mtroamd     # also wipes that state dir — full clean removal (iOS would need to re-pair)
sudo rm -f /etc/apt/sources.list.d/mtroamd-dev.list   # drop the dev source
```

`remove`/`purge` stop + disable your `--user` service and remove the unit for
the user who ran sudo; other users on the box manage their own.

## Go back to stable

The dev version sorts *below* the matching stable release, so just drop the dev
source and upgrade — apt converges you onto stable:

```sh
sudo rm /etc/apt/sources.list.d/mtroamd-dev.list
# add the stable line from the README, then:
sudo apt update && sudo apt install --only-upgrade mtroamd
```

(If you're on a `~rc` newer than the latest stable, `apt` won't "downgrade"
automatically — `sudo apt install mtroamd=<stable-version>` to pin back.)
