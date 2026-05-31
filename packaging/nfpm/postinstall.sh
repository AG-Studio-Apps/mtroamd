#!/bin/sh
# mtroamd .deb postinstall — per-user service model.
#
# Runs as root during `apt install`/`apt upgrade`. mtroamd is a systemd
# --user daemon that spawns shells AS the login user, so this script:
#   - NEVER creates a system user or installs a system unit,
#   - NEVER enables/starts a service for a user who hasn't opted in,
# and only:
#   (a) loudly flags DEVELOPMENT builds (dev apt channel),
#   (b) migrates a prior ~/.local/bin install (iOS app / `mtroamd update`)
#       onto this package binary, when it safely can,
#   (c) on a plain upgrade, restarts the installing user's already-running
#       --user service so it executes the new /usr/bin binary.
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

# (b) Prior ~/.local/bin install → migrate onto the package binary.
if [ -e "$old_bin" ] || [ -e "$old_unit" ]; then
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

# (c) No prior install. On an upgrade, cycle an already-running --user service
# so it picks up the new binary. try-restart is a no-op when not running, so a
# fresh first install never gets the service forced on it.
if bus_ok; then
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
	run_user systemctl --user try-restart mtroamd.service >/dev/null 2>&1 || true
fi
exit 0
