#!/bin/sh
# meshtermd RPM %post — per-user service model (Fedora/dnf).
#
# $1 = number of meshtermd versions installed after this transaction:
#   1 = fresh install,  >=2 = upgrade.
#
# Mirrors the .deb postinstall: flag dev builds, migrate a prior ~/.local/bin
# install for $SUDO_USER, and on upgrade restart that user's already-running
# --user service. Never creates a system user/unit or auto-enables a service.
set -eu

BIN=/usr/bin/meshtermd

# Dev-build warning — dev-channel RPMs carry a ~rc/~dev version.
ver="$(rpm -q --qf '%{VERSION}' meshtermd 2>/dev/null || true)"
case "$ver" in
*~*)
	cat >&2 <<'EOF'
⚠  meshtermd DEVELOPMENT build installed — unstable, may break your setup.
   Not for production. Use the stable channel for normal use.
EOF
	;;
esac

u="${SUDO_USER:-}"
if [ -z "$u" ] || [ "$u" = "root" ]; then
	echo "meshtermd: installed to $BIN. As your login user, enable it with:"
	echo "    systemctl --user enable --now meshtermd"
	exit 0
fi

uid="$(id -u "$u" 2>/dev/null || true)"
home="$(getent passwd "$u" | cut -d: -f6)"
rundir="/run/user/${uid}"
run_user() { runuser -u "$u" -- env XDG_RUNTIME_DIR="$rundir" "$@"; }
bus_ok() { [ -n "$uid" ] && [ -S "${rundir}/bus" ]; }

old_bin="${home}/.local/bin/meshtermd"
old_unit="${home}/.config/systemd/user/meshtermd.service"

# Prior ~/.local/bin install → migrate onto the package binary.
if [ -e "$old_bin" ] || [ -e "$old_unit" ]; then
	if bus_ok; then
		echo "meshtermd: migrating existing ~/.local/bin install for '$u' onto $BIN…"
		if ! run_user "$BIN" migrate --yes; then
			echo "meshtermd: auto-migration failed — run it yourself as '$u':  meshtermd migrate" >&2
		fi
	else
		echo "meshtermd: an existing ~/.local/bin install was detected for '$u',"
		echo "  but the user session isn't reachable from here. As '$u', run:"
		echo "      meshtermd migrate"
	fi
	exit 0
fi

# No prior install. On upgrade (>=2), cycle an already-running --user service so
# it executes the new binary. try-restart is a no-op when not running, so a
# fresh install never gets the service forced on it.
if [ "${1:-1}" -ge 2 ] 2>/dev/null && bus_ok; then
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
	run_user systemctl --user try-restart meshtermd.service >/dev/null 2>&1 || true
fi
exit 0
