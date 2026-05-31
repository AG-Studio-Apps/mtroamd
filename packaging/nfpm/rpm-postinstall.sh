#!/bin/sh
# mtroamd RPM %post — per-user service model (Fedora/dnf).
#
# $1 = number of mtroamd versions installed after this transaction:
#   1 = fresh install,  >=2 = upgrade.
#
# Mirrors the .deb postinstall: flag dev builds, migrate a prior ~/.local/bin
# install for $SUDO_USER, and on upgrade restart that user's already-running
# --user service. Never creates a system user/unit or auto-enables a service.
set -eu

BIN=/usr/bin/mtroamd

# Dev-build warning — dev-channel RPMs carry a ~rc/~dev version.
ver="$(rpm -q --qf '%{VERSION}' mtroamd 2>/dev/null || true)"
case "$ver" in
*~*)
	cat >&2 <<'EOF'
⚠  mtroamd DEVELOPMENT build installed — unstable, may break your setup.
   Not for production. Use the stable channel for normal use.
EOF
	;;
esac

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

# Prior ~/.local/bin install → migrate onto the package binary.
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

# No prior ~/.local/bin install. The package never auto-enables (opt-in only).
# $1: 1 = fresh install, >=2 = upgrade.
#   - upgrade: silently cycle the already-running --user service onto the new
#     binary (try-restart is a no-op if it isn't running).
#   - fresh:   print the exact paste-able command to start it, tailored to
#     whether the user bus is reachable (a session-less user also needs
#     enable-linger). Without this a fresh install left no next step.
if [ "${1:-1}" -ge 2 ] 2>/dev/null; then
	if bus_ok; then
		run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
		run_user systemctl --user try-restart mtroamd.service >/dev/null 2>&1 || true
	fi
elif bus_ok; then
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
	cat <<EOF
mtroamd: installed at $BIN. To start it now and on boot, run as '$u':

    systemctl --user enable --now mtroamd

  Check status any time with:  mtroamd doctor
EOF
else
	cat <<EOF
mtroamd: installed at $BIN — but '$u' has no active systemd --user session,
  so the service isn't running yet. Enable it (survives logout + reboot):

    sudo loginctl enable-linger $u
    sudo -u $u env XDG_RUNTIME_DIR=/run/user/$uid systemctl --user enable --now mtroamd

  Verify with: mtroamd doctor
  Or open this host in the meshTerm iOS app and choose "Reuse system binary".
EOF
fi
exit 0
