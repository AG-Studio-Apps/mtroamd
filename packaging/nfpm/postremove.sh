#!/bin/sh
# meshtermd .deb postremove — leave user state (~/.local/share/meshtermd:
# certs, sessions) untouched; only clean up an orphaned --user unit that
# points at the /usr/bin binary we just removed, so a later login doesn't see
# a ghost unit referencing a missing ExecStart.
set -eu

# Only on real removal, not the remove half of an upgrade.
[ "${1:-}" = "remove" ] || exit 0

u="${SUDO_USER:-}"
[ -n "$u" ] && [ "$u" != "root" ] || exit 0

home="$(getent passwd "$u" | cut -d: -f6)"
unit="${home}/.config/systemd/user/meshtermd.service"

# Only touch the unit if it actually points at the package binary — never
# disturb a user unit that's been re-pointed (e.g. back to ~/.local/bin).
if [ -e "$unit" ] && grep -q 'ExecStart=/usr/bin/meshtermd ' "$unit" 2>/dev/null; then
	uid="$(id -u "$u" 2>/dev/null || true)"
	rundir="/run/user/${uid}"
	if [ -n "$uid" ] && [ -S "${rundir}/bus" ]; then
		runuser -u "$u" -- env XDG_RUNTIME_DIR="$rundir" \
			systemctl --user disable --now meshtermd.service >/dev/null 2>&1 || true
	fi
	rm -f "$unit"
	echo "meshtermd: removed orphaned --user unit for '$u'"
fi
exit 0
