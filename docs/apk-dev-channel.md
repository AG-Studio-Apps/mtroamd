# mtroamd apk - development channel (Alpine)

> ⚠️ **DEVELOPMENT CHANNEL - UNSTABLE. MAY BREAK YOUR SYSTEM.**
> The `dev` suite carries pre-release builds from `develop` (versioned
> `…~rc`/`…~dev`). It pairs with the **TestFlight** meshTerm app, not the App
> Store release, and can ship protocol changes ahead of the stable daemon. Use
> it only for testing on a disposable host. **For normal use, follow the stable
> install in the [README](../README.md).**

Alpine has no systemd, so mtroamd runs as a system **OpenRC** service under a
configured user (the daemon spawns your shells AS that user).

## Opt in

```sh
# Trust the repo signing key.
curl -fsSL https://ag-studio-apps.github.io/mtroamd/mtroamd-apk.rsa.pub \
  | sudo tee /etc/apk/keys/mtroamd-apk.rsa.pub > /dev/null

# Add the dev suite for your arch (x86_64 / aarch64 / armv7).
echo "https://ag-studio-apps.github.io/mtroamd/apk/dev" \
  | sudo tee -a /etc/apk/repositories

sudo apk update && sudo apk add mtroamd

# Set MTROAMD_USER (and optionally MTROAMD_ADDR / MTROAMD_TCP_ADDR) - the
# daemon spawns shells AS this user, so make it your login user, not root on a
# multi-user box:
sudo vi /etc/conf.d/mtroamd

# Enable + start (survives reboot via OpenRC).
sudo rc-update add mtroamd
sudo rc-service mtroamd start

# Check it:
mtroamd doctor
```

The apk repo URL embeds the suite (`/apk/dev` vs `/apk/stable`); `apk` appends
the matching `<arch>/` automatically.

## Go back to stable

```sh
# Replace the dev line in /etc/apk/repositories with the stable one:
sudo sed -i 's#/mtroamd/apk/dev#/mtroamd/apk/stable#' /etc/apk/repositories
sudo apk update && sudo apk add --upgrade mtroamd
```

## Uninstall

```sh
sudo rc-service mtroamd stop
sudo rc-update del mtroamd
sudo apk del mtroamd
```

State in `<user>/.local/share/mtroamd` (cert + sessions) is kept; remove it by
hand for a full wipe (the iOS app would then need to re-pair).

## Note on the OpenRC service

`mtroamd doctor` reports the supervisor as `nohup` on Alpine (there's no
systemd `--user` bus and no OpenRC backend in the daemon's own detection yet) -
that's cosmetic; OpenRC supervises the process. A restart preserves running
sessions: `supervise-daemon` signals only the daemon, and the per-session
pty-sidecar children live in their own sessions (the OpenRC parallel to the
systemd unit's `KillMode=process`).
