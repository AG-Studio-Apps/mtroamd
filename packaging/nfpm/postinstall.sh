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
#   (c) on a fresh install, prints the exact command to start the service
#       (it never auto-enables); on an upgrade, restarts the installing
#       user's already-running --user service onto the new /usr/bin binary.
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

# (c) No prior ~/.local/bin install. The package deliberately never
# auto-enables a service (opt-in only). dpkg passes the previously-configured
# version in $2 — empty on a FRESH install, set on an UPGRADE:
#   - upgrade: silently cycle the already-running --user service onto the new
#     /usr/bin binary (try-restart is a no-op if it isn't running).
#   - fresh:   print the exact paste-able command to start it. Tailored to
#     whether the user bus is reachable from here, because a session-less /
#     headless user (no `/run/user/<uid>/bus`) also needs `enable-linger`.
#     Without this, a fresh `apt install` left the user with no next step
#     (and only systemd's own "no user bus" trigger noise).
if [ -n "${2:-}" ]; then
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
