#!/bin/sh
# mtroamd .deb postremove — clean, easy uninstall.
#
#   apt remove mtroamd  → stop + disable the installing user's --user service
#       and remove a migrate-created ~/.config unit pointing at the package
#       binary. User STATE (~/.local/share/mtroamd: cert, sessions) is KEPT
#       so a reinstall reuses the same identity (iOS pairing survives).
#   apt purge  mtroamd  → all of the above, plus wipe that state dir for a
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
	run_user systemctl --user stop mtroamd.service >/dev/null 2>&1 || true
	run_user systemctl --user disable mtroamd.service >/dev/null 2>&1 || true
	run_user systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

# Remove a migrate-created ~/.config unit that still points at the package
# binary (never disturb one re-pointed elsewhere, e.g. back to ~/.local/bin).
# Do the check + delete AS THE USER, not as root: the unit lives under the
# user's own $HOME, so a root `rm` here could be steered by a planted symlink
# (TOCTOU). Dropped to '$u', the worst a symlink can do is delete that user's
# own files — which they could do anyway. The path is passed as an argv arg
# ($1), never interpolated into the shell, so a hostile path can't inject.
unit="${home}/.config/systemd/user/mtroamd.service"
if [ -n "$home" ]; then
	if run_user sh -c '
		p=$1
		[ -e "$p" ] && grep -q "ExecStart=/usr/bin/mtroamd " "$p" 2>/dev/null && rm -f "$p"
	' sh "$unit" 2>/dev/null; then
		echo "mtroamd: removed --user unit for '$u'"
	fi
fi

# purge → wipe user state for a full clean removal. Again as the user: a root
# `rm -rf` walking a user-owned tree is a symlink/TOCTOU footgun, whereas as
# '$u' the worst case is the user deleting their own files. (Dropping the
# privilege is the fix; no `! -L` guard needed — `rm` never follows a symlink
# for the unlink, and we mirror the prior unconditional wipe.)
if [ "$action" = "purge" ] && [ -n "$home" ]; then
	if run_user sh -c '
		d=$1
		[ -e "$d" ] && rm -rf "$d"
	' sh "${home}/.local/share/mtroamd" 2>/dev/null; then
		echo "mtroamd: purged state dir for '$u' (cert + sessions removed)"
	fi
fi
exit 0
