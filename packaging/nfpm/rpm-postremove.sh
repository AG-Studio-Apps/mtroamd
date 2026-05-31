#!/bin/sh
# meshtermd RPM %postun — Fedora/dnf.
#
# $1 = number of meshtermd versions remaining after this transaction:
#   0 = final uninstall,  >=1 = upgrade (the new %post handles the restart, so
#   we must NOT tear down the service here).
#
# On final uninstall: stop + disable the installing user's --user service and
# remove a migrate-created ~/.config unit. User STATE (~/.local/share/meshtermd:
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
	run_user systemctl --user stop meshtermd.service >/dev/null 2>&1 || true
	run_user systemctl --user disable meshtermd.service >/dev/null 2>&1 || true
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

unit="${home}/.config/systemd/user/meshtermd.service"
if [ -n "$home" ] && [ -e "$unit" ] && grep -q 'ExecStart=/usr/bin/meshtermd ' "$unit" 2>/dev/null; then
	rm -f "$unit"
	echo "meshtermd: removed --user unit for '$u'"
fi
exit 0
