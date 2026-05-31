# meshtermd apt — development channel

> ⚠️ **DEVELOPMENT CHANNEL — UNSTABLE. MAY BREAK YOUR SYSTEM.**
> The `dev` suite carries pre-release builds from `develop` (versioned
> `…~rc`/`…~dev`). It pairs with the **TestFlight** meshTerm app, not the App
> Store release, and can ship protocol changes ahead of the stable daemon. Use
> it only for testing on a disposable host. **For normal use, follow the stable
> install in the [README](../README.md).**

## Opt in

```sh
curl -fsSL https://ag-studio-apps.github.io/meshtermd/meshtermd-archive-keyring.gpg \
  | sudo tee /usr/share/keyrings/meshtermd-archive-keyring.gpg > /dev/null

echo "deb [signed-by=/usr/share/keyrings/meshtermd-archive-keyring.gpg] https://ag-studio-apps.github.io/meshtermd dev main" \
  | sudo tee /etc/apt/sources.list.d/meshtermd-dev.list

sudo apt update && sudo apt install meshtermd
# then, as your login user:
systemctl --user enable --now meshtermd
```

Every dev install/upgrade prints a `⚠ DEVELOPMENT build` warning, and
`meshtermd --version` / `apt policy meshtermd` show the `~rc`/`~dev` suffix.

## Uninstall

```sh
sudo apt remove meshtermd     # stops + removes the daemon; keeps ~/.local/share/meshtermd (cert, sessions)
sudo apt purge  meshtermd     # also wipes that state dir — full clean removal (iOS would need to re-pair)
sudo rm -f /etc/apt/sources.list.d/meshtermd-dev.list   # drop the dev source
```

`remove`/`purge` stop + disable your `--user` service and remove the unit for
the user who ran sudo; other users on the box manage their own.

## Go back to stable

The dev version sorts *below* the matching stable release, so just drop the dev
source and upgrade — apt converges you onto stable:

```sh
sudo rm /etc/apt/sources.list.d/meshtermd-dev.list
# add the stable line from the README, then:
sudo apt update && sudo apt install --only-upgrade meshtermd
```

(If you're on a `~rc` newer than the latest stable, `apt` won't "downgrade"
automatically — `sudo apt install meshtermd=<stable-version>` to pin back.)
