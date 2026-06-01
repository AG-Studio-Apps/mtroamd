#!/bin/sh
# mtroamd RPM %postun — Fedora/dnf.
#
# $1 = number of mtroamd versions remaining after this transaction:
#   0 = final uninstall,  >=1 = upgrade (the new %post handles the restart, so
#   we must NOT tear down the service here).
#
# On final uninstall: stop + disable the installing user's --user service and
# remove a migrate-created ~/.config unit. User STATE (~/.local/share/mtroamd:
# cert, sessions) is kept — RPM has no `purge` step, so removal mirrors
# `apt remove` (not `apt purge`). Per-$SUDO_USER, best-effort.
set -eu

[ "${1:-1}" -eq 0 ] 2>/dev/null || exit 0

u="${SUDO_USER:-}"
[ -n "$u" ] && [ "$u" != "root" ] || exit 0
uid="$(id -u "$u" 2>/dev/null || true)"
home="$(getent passwd "$u" | cut -d: -f6)"
rundir="/run/user/${uid}"
run_user() { runuser -u "$u" -- env XDG_RUNTIME_DIR="$rundir" "$@"; }

if [ -n "$uid" ] && [ -S "${rundir}/bus" ]; then
	run_user systemctl --user stop mtroamd.service >/dev/null 2>&1 || true
	run_user systemctl --user disable mtroamd.service >/dev/null 2>&1 || true
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

# Remove a migrate-created ~/.config unit that still points at the package
# binary, AS THE USER (not root): the unit is under the user's own $HOME, so a
# root `rm` could follow a planted symlink (TOCTOU). Dropped to '$u' a symlink
# can only delete that user's own files. Path passed as argv ($1), never
# interpolated, so a hostile path can't inject.
unit="${home}/.config/systemd/user/mtroamd.service"
if [ -n "$home" ]; then
	if run_user sh -c '
		p=$1
		[ -e "$p" ] && grep -q "ExecStart=/usr/bin/mtroamd " "$p" 2>/dev/null && rm -f "$p"
	' sh "$unit" 2>/dev/null; then
		echo "mtroamd: removed --user unit for '$u'"
	fi
fi
exit 0
