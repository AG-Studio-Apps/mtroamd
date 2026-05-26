#!/bin/bash
# meshtermd install diagnostic — run as the target user on the host
set -u

echo "=== meshtermd Install Diagnostic ==="
echo "Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "User: $(id)"
echo "Home: $HOME"
echo ""

echo "--- Binary ---"
which meshtermd 2>/dev/null || echo "(not in PATH)"
ls -la ~/.local/bin/meshtermd 2>/dev/null || echo "(not installed at ~/.local/bin/meshtermd)"
~/.local/bin/meshtermd version 2>/dev/null || echo "(binary missing or won't run)"
echo ""

echo "--- State directory ---"
ls -la ~/.local/share/meshtermd/ 2>/dev/null || echo "(state dir does not exist)"
echo ""

echo "--- Daemon log ---"
tail -30 ~/.local/share/meshtermd/meshtermd.log 2>/dev/null || echo "(no log file)"
echo ""

echo "--- Start script ---"
cat ~/.local/share/meshtermd/start.sh 2>/dev/null || echo "(no start.sh)"
echo ""

echo "--- Port state files ---"
cat ~/.local/share/meshtermd/quic-port 2>/dev/null || echo "(no quic-port file)"
cat ~/.local/share/meshtermd/tcp-port 2>/dev/null || echo "(no tcp-port file)"
echo ""

echo "--- Socket ---"
ls -la ~/.local/share/meshtermd/meshtermd.sock 2>/dev/null || echo "(no socket)"
echo ""

echo "--- Status ---"
~/.local/bin/meshtermd status 2>&1 || true
echo ""

echo "--- Processes ---"
ps -u $(id -u) -o pid,args 2>/dev/null | grep meshtermd | grep -v grep || echo "(no meshtermd processes)"
echo ""

echo "--- Ports ---"
ss -tlnp 2>/dev/null | grep -E '4982|4992' || echo "(no meshtermd ports bound)"
ss -ulnp 2>/dev/null | grep -E '4982|4992' || echo "(no meshtermd UDP ports bound)"
echo ""

echo "--- Systemd user bus ---"
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus
test -S /run/user/$(id -u)/bus && echo "bus socket exists" || echo "NO bus socket"
systemctl --user is-system-running 2>&1 || echo "(systemd user bus unreachable)"
echo ""

echo "--- Systemd unit ---"
systemctl --user status meshtermd 2>&1 || true
echo ""

echo "--- Systemd journal ---"
journalctl --user -u meshtermd --no-pager -n 20 2>&1 || echo "(journal unavailable)"
echo ""

echo "--- Unit file ---"
cat ~/.config/systemd/user/meshtermd.service 2>/dev/null || echo "(no unit file)"
echo ""

echo "--- Linger ---"
loginctl show-user "$(id -un)" --property=Linger 2>/dev/null || echo "(linger check failed)"
echo ""

echo "=== Done ==="
