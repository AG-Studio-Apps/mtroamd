#!/bin/bash
# mtroamd install diagnostic — run as the target user on the host
set -u

echo "=== mtroamd Install Diagnostic ==="
echo "Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "User: $(id)"
echo "Home: $HOME"
echo ""

echo "--- Binary ---"
which mtroamd 2>/dev/null || echo "(not in PATH)"
ls -la ~/.local/bin/mtroamd 2>/dev/null || echo "(not installed at ~/.local/bin/mtroamd)"
~/.local/bin/mtroamd version 2>/dev/null || echo "(binary missing or won't run)"
echo ""

echo "--- State directory ---"
ls -la ~/.local/share/mtroamd/ 2>/dev/null || echo "(state dir does not exist)"
echo ""

echo "--- Daemon log ---"
tail -30 ~/.local/share/mtroamd/mtroamd.log 2>/dev/null || echo "(no log file)"
echo ""

echo "--- Start script ---"
cat ~/.local/share/mtroamd/start.sh 2>/dev/null || echo "(no start.sh)"
echo ""

echo "--- Port state files ---"
cat ~/.local/share/mtroamd/quic-port 2>/dev/null || echo "(no quic-port file)"
cat ~/.local/share/mtroamd/tcp-port 2>/dev/null || echo "(no tcp-port file)"
echo ""

echo "--- Socket ---"
ls -la ~/.local/share/mtroamd/mtroamd.sock 2>/dev/null || echo "(no socket)"
echo ""

echo "--- Status ---"
~/.local/bin/mtroamd status 2>&1 || true
echo ""

echo "--- Processes ---"
ps -u $(id -u) -o pid,args 2>/dev/null | grep mtroamd | grep -v grep || echo "(no mtroamd processes)"
echo ""

echo "--- Ports ---"
ss -tlnp 2>/dev/null | grep -E '4982|4992' || echo "(no mtroamd ports bound)"
ss -ulnp 2>/dev/null | grep -E '4982|4992' || echo "(no mtroamd UDP ports bound)"
echo ""

echo "--- Systemd user bus ---"
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus
test -S /run/user/$(id -u)/bus && echo "bus socket exists" || echo "NO bus socket"
systemctl --user is-system-running 2>&1 || echo "(systemd user bus unreachable)"
echo ""

echo "--- Systemd unit ---"
systemctl --user status mtroamd 2>&1 || true
echo ""

echo "--- Systemd journal ---"
journalctl --user -u mtroamd --no-pager -n 20 2>&1 || echo "(journal unavailable)"
echo ""

echo "--- Unit file ---"
cat ~/.config/systemd/user/mtroamd.service 2>/dev/null || echo "(no unit file)"
echo ""

echo "--- Linger ---"
loginctl show-user "$(id -un)" --property=Linger 2>/dev/null || echo "(linger check failed)"
echo ""

echo "=== Done ==="
