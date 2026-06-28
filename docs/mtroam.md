# mtroam — the laptop CLI for mtroamd

`mtroam` is the desktop/laptop companion to `mtroamd`. It speaks the
same mtRoam protocol the iOS meshTerm app speaks, but renders the remote
session in your local terminal instead of an on-device view.

Use it when you want:

- Persistent shell sessions across SSH drops, sleeps, and network
  changes — the same value mtRoam gives iOS users
- The same sessions reachable from iOS *and* the laptop, so you can
  start a build on your phone in the morning and reattach from the
  laptop in the afternoon (or vice versa)
- Tier-1 management of remote daemons (list / kill / rename / status)
  without opening a separate SSH window

`mtroam` is **not**:

- An SSH client. It shells out to your system `ssh` for the bootstrap
  step, inheriting `~/.ssh/config`, `ssh-agent`, ProxyCommand, and
  ControlMaster multiplexing. If `ssh user@host` works, `mtroam` works.
- A replacement for `mtroamd`. The daemon still needs to be running
  on the remote host — `mtroam` is a client only.

## Install

Pick the right binary for your laptop's OS + arch from the latest
release: <https://github.com/AG-Studio-Apps/mtroamd/releases/latest>

```bash
# Linux amd64 example. Swap the asset filename for your platform.
PLATFORM=linux-amd64

cd /tmp && rm -rf mtroam-install && mkdir mtroam-install && cd mtroam-install
curl -fLO https://github.com/AG-Studio-Apps/mtroamd/releases/latest/download/mtroam-${PLATFORM}
curl -fLO https://github.com/AG-Studio-Apps/mtroamd/releases/latest/download/SHA256SUMS
curl -fLO https://github.com/AG-Studio-Apps/mtroamd/releases/latest/download/SHA256SUMS.minisig
curl -fLO https://raw.githubusercontent.com/AG-Studio-Apps/mtroamd/main/docs/release-public-key.txt

# Verify signature (one-time: sudo apt install minisign / brew install minisign)
minisign -V -p release-public-key.txt -m SHA256SUMS
# Expect: "Signature and comment signature verified — Trusted comment: mtroamd vX.Y.Z"

# Verify this asset's hash
sha256sum -c SHA256SUMS --ignore-missing 2>&1 | grep mtroam-${PLATFORM}
# Expect: "mtroam-linux-amd64: OK"

# Install
mkdir -p ~/.local/bin
install -m 755 mtroam-${PLATFORM} ~/.local/bin/mtroam
mtroam version
```

Make sure `~/.local/bin` is on your `$PATH`.

After the initial install, future upgrades are one command:

```bash
mtroam update            # check + apply if available
mtroam update --check    # check only; exit 0 = up to date, 1 = available
```

## Attach to a remote session

```bash
mtroam attach user@host new                  # spawn + attach to a fresh shell
mtroam attach user@host my-session           # attach to "my-session", create if missing
mtroam attach user@host <hex-id>             # reattach to a specific session by id
mtroam attach user@host --mode readonly <id> # watcher: see output, can't type
mtroam attach user@host --mode exclusive-if-free <id> # polite: exclusive if free, else watcher
```

If you always attach to the same host, set `$MTROAM_HOST` (or write it
to `~/.config/mtroam/host`) and drop the `user@host` argument:

```bash
export MTROAM_HOST=user@example.com
mtroam attach my-session
```

While attached:

- **Detach**: type `~.` on a fresh line. The remote shell stays alive
  on the daemon; reattach with the same command any time.
- **Window resize**: handled automatically — your local terminal's
  size changes are forwarded as Resize frames.
- **Reconnect on drop**: if your network blips, the local pump exits
  cleanly. Re-run the same `mtroam attach` to pick up where you left
  off; the daemon replays missed output.

## Manage remote sessions

All Tier 1 commands accept the same `--host`/`$MTROAM_HOST` shape as
attach:

```bash
mtroam list user@host                # all sessions on this daemon
mtroam list user@host --json         # machine-readable; same wire shape iOS consumes
mtroam status user@host              # daemon-wide snapshot (QUIC addr, sessions, idle)
mtroam session-info user@host <id>   # one session: rows, cols, idle, attached clients
mtroam rename user@host <id> new-name
mtroam kill user@host <id-or-name>   # reap; PTY + buffer go away
mtroam new user@host --name backend  # create without attaching
mtroam search user@host <id> <regex> # grep a session's scrollback
mtroam tail user@host <id>           # passive watch — invisible to other clients
mtroam doctor user@host              # daemon/host health snapshot
mtroam restart user@host             # save sessions & restart the daemon
```

## Self-update

`mtroam update` mirrors `mtroamd update`:

- Resolves the latest signed release (or `--tag X.Y.Z`) via the
  GitHub Releases API
- Verifies the SHA256SUMS minisign signature against the same primary
  + emergency key roster the iOS app uses
- Verifies the binary's SHA-256 against the signed manifest
- Atomically swaps your `~/.local/bin/mtroam`

Anti-rollback is on by default: `mtroam update --tag <older-version>`
refuses unless you pass `--allow-downgrade`.

Exit codes match `mtroamd update`:

| Code | Meaning                                                |
|------|--------------------------------------------------------|
| 0    | up to date OR update succeeded                         |
| 1    | update available (only with `--check`)                 |
| 2    | bad flags / user cancelled                             |
| 3    | verification failed (security event)                   |
| 4    | download / network failure                             |

## Uninstall

```bash
mtroam uninstall              # confirms y/N, removes ~/.local/bin/mtroam
mtroam uninstall --yes        # non-interactive
```

mtroam has no state directory of its own (no cert, no key, no socket),
so there's no `--purge` equivalent.

## Troubleshooting

**"command not found: mtroam"** — `~/.local/bin` isn't on your `$PATH`.
Either add it (`export PATH="$HOME/.local/bin:$PATH"` in your shell rc)
or move the binary somewhere already on `$PATH`.

**"mtroam attach: bootstrap: …"** — the SSH layer failed. Run the same
`ssh user@host` invocation manually to see why (auth failure, host
unreachable, etc.).

**"mtroam attach: bootstrap: command not found: mtroamd"** — the
daemon isn't installed on the remote host. Use the meshTerm iOS app's
"Set Up mtRoam on this Host" flow to install it, then try again.

**"mtroam attach: tls: certificate signed by unknown authority"** — the
daemon's TLS cert fingerprint doesn't match what the bootstrap line
declared. Likely a man-in-the-middle on the QUIC port, or the daemon
regenerated its cert between SSH bootstrap and your QUIC dial (rare).
Don't continue.

**"mtroam attach: ErrNotATerminal"** — stdin isn't a TTY. mtroam needs a
terminal to drive raw mode + window-size queries; running it from a
script with redirected stdin won't work.

**"failed to sufficiently increase receive buffer size" in the daemon
log**: a benign quic-go notice, not a real error. It asked for a larger
UDP receive buffer than the OS default (`net.core.rmem_max`) permits,
which is harmless for terminal traffic (low bandwidth, bursty), so no
action is needed. If you run `mtroamd` as a high-throughput QUIC endpoint
and want the extra headroom, raise it on the host, e.g.
`sudo sysctl -w net.core.rmem_max=7500000` (persist via a
`/etc/sysctl.d/` drop-in).

## Compatibility

`mtroam` and `mtroamd` ship in the same release. Pair `mtroam-vX.Y.Z`
with a daemon at `mtroamd-vX.Y.Z` or newer — older daemons may not
understand newer wire fields. The iOS app pins its own daemon version
independently via the auto-installer's `pinnedReleaseTag`.
