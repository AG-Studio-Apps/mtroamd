#!/bin/sh
# meshtermd .deb postremove — clean, easy uninstall.
#
#   apt remove meshtermd  → stop + disable the installing user's --user service
#       and remove a migrate-created ~/.config unit pointing at the package
#       binary. User STATE (~/.local/share/meshtermd: cert, sessions) is KEPT
#       so a reinstall reuses the same identity (iOS pairing survives).
#   apt purge  meshtermd  → all of the above, plus wipe that state dir for a
#       full clean removal (cert goes → iOS would need to re-pair).
#
# Per-user: only acts for $SUDO_USER (other users manage their own service);
# everything is best-effort so package removal never fails.
set -eu

action="${1:-}"
case "$action" in
remove | purge) : ;;
*) exit 0 ;; # upgrade/abort-*/disappear: nothing to tear down here
esac

u="${SUDO_USER:-}"
[ -n "$u" ] && [ "$u" != "root" ] || exit 0
uid="$(id -u "$u" 2>/dev/null || true)"
home="$(getent passwd "$u" | cut -d: -f6)"
rundir="/run/user/${uid}"
run_user() { runuser -u "$u" -- env XDG_RUNTIME_DIR="$rundir" "$@"; }
bus_ok() { [ -n "$uid" ] && [ -S "${rundir}/bus" ]; }

# Stop + disable the running service for the installing user (best-effort).
# Covers both a packaged-unit enable and a migrate-created ~/.config unit;
# `stop` catches an in-memory instance even after dpkg removed the unit file.
if bus_ok; then
	run_user systemctl --user stop meshtermd.service >/dev/null 2>&1 || true
	run_user systemctl --user disable meshtermd.service >/dev/null 2>&1 || true
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

# Remove a migrate-created ~/.config unit that still points at the package
# binary (never disturb one re-pointed elsewhere, e.g. back to ~/.local/bin).
unit="${home}/.config/systemd/user/meshtermd.service"
if [ -n "$home" ] && [ -e "$unit" ] && grep -q 'ExecStart=/usr/bin/meshtermd ' "$unit" 2>/dev/null; then
	rm -f "$unit"
	echo "meshtermd: removed --user unit for '$u'"
fi

# purge → wipe user state for a full clean removal.
if [ "$action" = "purge" ] && [ -n "$home" ]; then
	rm -rf "${home}/.local/share/meshtermd"
	echo "meshtermd: purged state dir for '$u' (cert + sessions removed)"
fi
exit 0
