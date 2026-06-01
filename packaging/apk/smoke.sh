#!/bin/sh
# Smoke test for the mtroamd Alpine apk: install → OpenRC start → create a
# session → restart → assert the pty-sidecar survived and the daemon reattached
# it. Runs inside a privileged alpine container.
#
# Two install modes:
#   MTROAMD_APK=/path/to/mtroamd-*.apk   → install that local package
#                                          (used as a release gate in CI)
#   (unset)                              → install from the LIVE dev apk repo
#                                          (manual end-to-end check)
#
# Exits non-zero on any failed assertion.
set -u
PASS=0; FAIL=0
ok(){  echo "  PASS: $*"; PASS=$((PASS+1)); }
bad(){ echo "  FAIL: $*"; FAIL=$((FAIL+1)); }
SOCK=/root/.local/share/mtroamd/mtroamd.sock
LOG=/var/log/mtroamd.log

echo "### install"
if [ -n "${MTROAMD_APK:-}" ]; then
  apk add --no-progress --allow-untrusted "$MTROAMD_APK" 2>&1 | tail -4 \
    && ok "apk add (local: $MTROAMD_APK)" || bad "apk add local apk"
else
  wget -qO /etc/apk/keys/mtroamd-apk.rsa.pub \
    https://ag-studio-apps.github.io/mtroamd/mtroamd-apk.rsa.pub || bad "fetch key"
  echo "https://ag-studio-apps.github.io/mtroamd/apk/dev" >> /etc/apk/repositories
  apk -q update >/dev/null 2>&1
  apk add --no-progress mtroamd 2>&1 | tail -4 \
    && ok "apk add (live dev repo)" || bad "apk add from dev repo"
fi
apk -q add openrc >/dev/null 2>&1

echo "### files + binary"
for f in /usr/bin/mtroamd /etc/init.d/mtroamd /etc/conf.d/mtroamd \
         /usr/share/man/man8/mtroamd.8.gz; do
  [ -e "$f" ] && ok "exists ${f##*/}" || bad "missing $f"
done
mtroamd version >/dev/null 2>&1 && ok "mtroamd version (musl)" || bad "mtroamd version"

echo "### OpenRC start"
mkdir -p /run/openrc 2>/dev/null; touch /run/openrc/softlevel 2>/dev/null || true
rc-service mtroamd start 2>&1 | tail -3 || true
sleep 2
pgrep -f 'mtroamd serve' >/dev/null 2>&1 && ok "daemon running under OpenRC" \
  || bad "daemon not running via rc-service"

echo "### session create + sidecar survival + reattach across restart"
mtroamd connect --socket "$SOCK" --session new 2>&1 | grep -q MTRM_QUIC \
  && ok "session created" || bad "create session"
sid="$(mtroamd list --socket "$SOCK" 2>/dev/null | awk 'NR==2{print $2}')"
before="$(pgrep -f 'pty-sidecar' 2>/dev/null | sort | tr '\n' ' ')"
[ -n "$before" ] && ok "pty-sidecar spawned [$before]" || bad "no pty-sidecar"

rc-service mtroamd restart 2>&1 | tail -2 || true
# Poll: the daemon must come back AND re-list the session (persistence settles
# a beat after restart — don't race it).
reattached=0
i=0; while [ "$i" -lt 15 ]; do
  if mtroamd list --socket "$SOCK" 2>/dev/null | grep -q .; then
    mtroamd list --socket "$SOCK" 2>/dev/null | grep -q "session-" && { reattached=1; break; }
  fi
  sleep 1; i=$((i + 1))
done
after="$(pgrep -f 'pty-sidecar' 2>/dev/null | sort | tr '\n' ' ')"

[ -n "$before" ] && [ "$before" = "$after" ] \
  && ok "pty-sidecar SURVIVED restart [$after]" \
  || bad "sidecar did not survive (before=[$before] after=[$after])"
[ "$reattached" = 1 ] \
  && ok "daemon reattached the session after restart" \
  || bad "session not re-listed after restart"
grep -q 'session.sidecar.reattached' "$LOG" 2>/dev/null \
  && ok "log confirms sidecar.reattached" \
  || echo "  note: 'sidecar.reattached' not in $LOG (informational)"

echo; echo "### SUMMARY  PASS=$PASS  FAIL=$FAIL"
[ "$FAIL" = 0 ]
