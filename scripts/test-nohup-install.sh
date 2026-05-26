#!/bin/bash
set -u
echo "=== Test nohup install sequence ==="

BINPATH="$HOME/.local/bin/meshtermd"
STATEDIR="$HOME/.local/share/meshtermd"
SOCKET="$STATEDIR/meshtermd.sock"
LOGFILE="$STATEDIR/meshtermd.log"
SCRIPT="$STATEDIR/start.sh"
QUIC="0.0.0.0:49820"
TCP="127.0.0.1:49920"

echo "1. Clean slate"
rm -rf "$STATEDIR"
ls -la "$STATEDIR" 2>/dev/null || echo "   (state dir removed)"

echo ""
echo "2. Kill stale"
pkill -u "$(id -u)" -f 'meshtermd serve' 2>/dev/null; sleep 1
pkill -9 -u "$(id -u)" -f 'meshtermd serve' 2>/dev/null; sleep 1
echo "   done"

echo ""
echo "3. Create state dir"
mkdir -p "$STATEDIR" && chmod 700 "$STATEDIR"
echo "   exit=$?"
ls -la "$STATEDIR/"

echo ""
echo "4. Write start script"
cat > "$SCRIPT" << 'EOF'
#!/bin/sh
BINPATH_PH serve --addr QUIC_PH --roam-tcp-addr TCP_PH --socket SOCKET_PH
EOF
# Replace placeholders
sed -i "s|BINPATH_PH|nohup $BINPATH|" "$SCRIPT"
sed -i "s|QUIC_PH|$QUIC|" "$SCRIPT"
sed -i "s|TCP_PH|$TCP|" "$SCRIPT"
sed -i "s|SOCKET_PH|$SOCKET </dev/null >>$LOGFILE 2>\&1 \&|" "$SCRIPT"
chmod +x "$SCRIPT"
echo "   Script contents:"
cat "$SCRIPT"

echo ""
echo "5. Run start script"
"$SCRIPT"
sleep 3

echo ""
echo "6. Check results"
echo "   Processes:"
ps -u "$(id -u)" -o pid,args 2>/dev/null | grep meshtermd | grep -v grep || echo "   (none)"
echo "   Status:"
"$BINPATH" status --socket "$SOCKET" 2>&1
echo "   Log:"
cat "$LOGFILE" 2>/dev/null || echo "   (no log)"
echo "   Ports:"
ss -tlnp 2>/dev/null | grep -E '4982|4992' || echo "   (none)"
ss -ulnp 2>/dev/null | grep -E '4982|4992' || echo "   (none)"

echo ""
echo "=== Done ==="
