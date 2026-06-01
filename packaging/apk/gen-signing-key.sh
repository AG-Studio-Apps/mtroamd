#!/usr/bin/env bash
# Generate the mtroamd apk repo signing key and store the PRIVATE half as the
# GitHub Actions secret `ABUILD_KEY`. The CI `apk-repo` job (release.yml) writes
# that key to ~/.abuild/mtroamd-apk.rsa, derives the public half, signs every
# .apk + the APKINDEX, and publishes the public key to gh-pages so `apk` can
# verify the repo.
#
# abuild signing keys are ordinary RSA keys, so this uses openssl (no Alpine /
# abuild needed). Run it once. Re-running reuses the existing key unless you
# delete it.
#
# Usage:   ./packaging/apk/gen-signing-key.sh
# Env:     REPO     (default AG-Studio-Apps/mtroamd)
#          OUT_DIR  (default ~/.mtroamd-apk-key)
set -euo pipefail

REPO="${REPO:-AG-Studio-Apps/mtroamd}"
OUT_DIR="${OUT_DIR:-$HOME/.mtroamd-apk-key}"
PRIV="$OUT_DIR/mtroamd-apk.rsa"
PUB="$PRIV.pub"

command -v openssl >/dev/null || { echo "error: openssl is required" >&2; exit 1; }
command -v gh >/dev/null      || { echo "error: gh (GitHub CLI) is required" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: run 'gh auth login' first" >&2; exit 1; }

mkdir -p "$OUT_DIR"; chmod 700 "$OUT_DIR"

if [ -f "$PRIV" ]; then
	echo "Reusing existing key: $PRIV  (delete it to regenerate)"
else
	echo "Generating 4096-bit RSA signing key → $PRIV"
	# PKCS#1 traditional RSA — the format abuild-sign expects.
	openssl genrsa -out "$PRIV" 4096 2>/dev/null
	chmod 600 "$PRIV"
fi

# Public half — informational locally; CI derives its own copy from the secret.
openssl rsa -in "$PRIV" -pubout -out "$PUB" 2>/dev/null

echo "Uploading private key as secret 'ABUILD_KEY' on $REPO …"
gh secret set ABUILD_KEY --repo "$REPO" < "$PRIV"

cat <<EOF

Done.
  Private key : $PRIV
                ^ KEEP SAFE + back it up — losing it means you can't sign repo
                  updates (users would have to trust a new key).
  Public key  : $PUB
  Secret      : ABUILD_KEY set on $REPO

Next: cut a -rc tag to publish to the dev apk suite, then verify on Alpine.
EOF
