#!/bin/sh
# mtroamd .deb postinstall — per-user service model.
#
# Runs as root during `apt install`/`apt upgrade`. mtroamd is a systemd
# --user daemon that spawns shells AS the login user, so this script never
# creates a system user or installs a system-wide unit. What it does:
#   (a) loudly flags DEVELOPMENT builds (dev apt channel),
#   (b) migrates a prior ~/.local/bin install (iOS app / `mtroamd update`)
#       onto this package binary, when it safely can,
#   (c) on a FRESH install, fully sets up the installing user's --user service
#       (enable-linger + enable --now) so it works with no manual step and
#       survives logout + reboot; on an UPGRADE, cycles that user's
#       already-running service onto the new /usr/bin binary without changing
#       their opt-in state.
set -eu

BIN=/usr/bin/mtroamd

# (a) Dev-build warning — the dev-channel .deb carries a ~rc/~dev tilde version.
ver="$(dpkg-query -W -f='${Version}' mtroamd 2>/dev/null || true)"
case "$ver" in
*~*)
	cat >&2 <<'EOF'
⚠  mtroamd DEVELOPMENT build installed — unstable, may break your setup.
   Not for production. Use the stable channel for normal use.
EOF
	;;
esac

# Everything below targets the human who ran sudo. Without $SUDO_USER (root
# login / automation) we can't act on a per-user service — print guidance.
u="${SUDO_USER:-}"
if [ -z "$u" ] || [ "$u" = "root" ]; then
	echo "mtroamd: installed to $BIN. As your login user, enable it with:"
	echo "    systemctl --user enable --now mtroamd"
	exit 0
fi

uid="$(id -u "$u" 2>/dev/null || true)"
home="$(getent passwd "$u" | cut -d: -f6)"
rundir="/run/user/${uid}"
run_user() { runuser -u "$u" -- env XDG_RUNTIME_DIR="$rundir" "$@"; }
bus_ok() { [ -n "$uid" ] && [ -S "${rundir}/bus" ]; }

old_bin="${home}/.local/bin/mtroamd"
old_unit="${home}/.config/systemd/user/mtroamd.service"

# (b) Prior ~/.local/bin install → migrate onto the package binary. Trigger
# ONLY when there is genuinely something to migrate: the old binary still
# exists, OR the active unit still launches mtroamd from ~/.local/bin. The
# unit FILE lives at $old_unit for package installs too, so its mere presence
# is NOT a migration signal — keying on it re-ran a no-op `migrate` on every
# upgrade and exited before the restart in (c), leaving the new binary on disk
# while the old one kept running (until a manual `mtroamd restart`).
if [ -f "$old_bin" ] || { [ -e "$old_unit" ] && grep -q 'ExecStart=.*/\.local/bin/mtroamd ' "$old_unit" 2>/dev/null; }; then
	if bus_ok; then
		echo "mtroamd: migrating existing ~/.local/bin install for '$u' onto $BIN…"
		if ! run_user "$BIN" migrate --yes; then
			echo "mtroamd: auto-migration failed — run it yourself as '$u':  mtroamd migrate" >&2
		fi
	else
		echo "mtroamd: an existing ~/.local/bin install was detected for '$u',"
		echo "  but the user session isn't reachable from here. As '$u', run:"
		echo "      mtroamd migrate"
	fi
	exit 0
fi

# (c) No prior ~/.local/bin install. dpkg passes the previously-configured
# version in $2 — empty on a FRESH install, set on an UPGRADE:
#   - upgrade: silently cycle the already-running --user service onto the new
#     /usr/bin binary (try-restart is a no-op if it isn't running). We never
#     change a user's opt-in/linger state on upgrade.
#   - fresh:   fully set the service up (below).
if [ -n "${2:-}" ]; then
	if bus_ok; then
		run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
		run_user systemctl --user try-restart mtroamd.service >/dev/null 2>&1 || true
	fi
	exit 0
fi

# Fresh install. mtroamd is a roaming daemon meant to outlive SSH logout +
# reboot, so set it up completely for '$u' — no paste-and-run step left behind.

# Skip the systemd dance entirely on hosts without it (containers, chroots,
# WSL1, minimal images) — no point enabling linger or polling for a user bus
# that can never appear, and it avoids a pointless 5s wait + misleading hint.
if ! command -v systemctl >/dev/null 2>&1; then
	echo "mtroamd: installed at $BIN. No systemd here — start it with whatever"
	echo "  supervisor this host uses (or: mtroamd serve)."
	exit 0
fi

# Enable linger first, as root: unlike a user enabling their own linger this
# needs no polkit prompt, and it spins up user@<uid>.service, which CREATES
# /run/user/<uid>/bus — so even a session-less / headless user becomes
# reachable here. Best-effort: a linger failure must never fail the install.
loginctl enable-linger "$u" >/dev/null 2>&1 || true

# enable-linger brings the user manager up asynchronously; wait briefly (≤5s)
# for the bus socket before driving `systemctl --user`. (Fractional `sleep` is
# a GNU/busybox coreutils extension, not POSIX, but both Debian and Fedora
# ship it.)
i=0
while [ "$i" -lt 25 ] && ! bus_ok; do
	sleep 0.2
	i=$((i + 1))
done

if bus_ok; then
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
	if run_user systemctl --user enable --now mtroamd.service >/dev/null 2>&1; then
		cat <<EOF
mtroamd: installed at $BIN and started for '$u' — enabled on boot and set to
  survive logout (linger). Check status any time with:  mtroamd doctor
EOF
		exit 0
	fi
fi

# Bus never came up, or enable failed — never leave the user stranded; print
# the manual steps as a fallback.
cat <<EOF
mtroamd: installed at $BIN, but couldn't auto-start the --user service for '$u'.
  Enable it manually (survives logout + reboot):

    sudo loginctl enable-linger $u
    sudo -u $u env XDG_RUNTIME_DIR=/run/user/$uid systemctl --user enable --now mtroamd

  Verify with: mtroamd doctor
  Or open this host in the meshTerm iOS app and choose "Reuse system binary".
EOF
exit 0
