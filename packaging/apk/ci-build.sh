#!/bin/sh
# Runs INSIDE an alpine container from the release.yml `apk-repo` job. Builds +
# signs the mtroamd apks for every arch by repackaging the prebuilt static
# dist/ binaries (no compilation). abuild refuses to run as root, so the build
# loop runs as an unprivileged `builder` user; key setup + index signing run as
# root. Inputs (env / mounted at /work):
#   MTROAMD_VERSION        → apk pkgver
#   /work/dist             → the prebuilt mtroamd-/mtroam-linux-<arch> + man + completions
#   /work/LICENSE          → license file
#   /work/abuild-key.rsa   → RSA signing private key (from the ABUILD_KEY secret)
#   /work/packaging/apk    → APKBUILD + initd/confd
# Output: /work/apkout/<arch>/{*.apk,APKINDEX.tar.gz} + /work/apkout/mtroamd-apk.rsa.pub
set -eu

: "${MTROAMD_VERSION:?MTROAMD_VERSION must be set}"

# Alpine pkgver can't carry semver prerelease syntax (no '-', no dotted
# suffix). Map "-rc.N" → "_rcN" — the apk pre-release form, which also sorts
# below the matching stable release (same intent as the deb/rpm "~rc").
MTROAMD_VERSION="$(printf '%s' "$MTROAMD_VERSION" | sed -E 's/-rc\.?([0-9]+).*/_rc\1/')"
echo ">>> apk pkgver: $MTROAMD_VERSION"

apk add --no-cache alpine-sdk openssl doas >/dev/null
# Let the abuild group install build-deps via doas (none here, but abuild's
# dep phase still expects a privilege helper).
echo "permit nopass :abuild" > /etc/doas.d/abuild.conf

adduser -D -G abuild builder

# Signing key under builder's ~/.abuild with a FIXED name → stable apk keyname.
mkdir -p /home/builder/.abuild /etc/apk/keys
install -m600 /work/abuild-key.rsa /home/builder/.abuild/mtroamd-apk.rsa
openssl rsa -in /home/builder/.abuild/mtroamd-apk.rsa -pubout \
  -out /home/builder/.abuild/mtroamd-apk.rsa.pub 2>/dev/null
cp /home/builder/.abuild/mtroamd-apk.rsa.pub /etc/apk/keys/
echo "PACKAGER_PRIVKEY=/home/builder/.abuild/mtroamd-apk.rsa" > /home/builder/.abuild/abuild.conf

# `abuild checksum` rewrites the APKBUILD → operate on a builder-owned copy.
cp -r /work/packaging/apk /home/builder/apkbuild
chown -R builder:abuild /home/builder

# Build each arch as the unprivileged user. No compilation: package() installs
# the matching prebuilt binary from $MTROAMD_DIST.
su builder -c '
  set -u
  export MTROAMD_VERSION="'"$MTROAMD_VERSION"'"
  export MTROAMD_DIST=/work/dist
  export MTROAMD_LICENSE=/work/LICENSE
  cd /home/builder/apkbuild
  abuild checksum
  # Per-arch, best-effort: a single arch failing (e.g. a foreign CARCH abuild
  # quirk) must not sink the others — collection below fails only if NOTHING
  # built. CARCH is overridden because package() just repackages the matching
  # prebuilt static binary (no compilation).
  for pair in "x86_64 amd64" "aarch64 arm64" "armv7 armv7"; do
    set -- $pair
    echo ">>> apk build: CARCH=$1 GOARCH=$2"
    CARCH="$1" MTROAMD_GOARCH="$2" abuild -r || echo ">>> WARN: abuild failed for CARCH=$1"
  done
'

# Collect, index, and sign per arch (root is fine for these).
total=0
for arch in x86_64 aarch64 armv7; do
  out="/work/apkout/$arch"
  mkdir -p "$out"
  found=0
  for f in /home/builder/packages/*/"$arch"/*.apk; do
    [ -e "$f" ] || continue
    cp "$f" "$out/"
    found=1
  done
  if [ "$found" != 1 ]; then
    rmdir "$out"
    continue
  fi
  total=$((total + 1))
  ( cd "$out"
    apk index -o APKINDEX.tar.gz ./*.apk
    abuild-sign -k /home/builder/.abuild/mtroamd-apk.rsa APKINDEX.tar.gz )
done

if [ "$total" = 0 ]; then
  echo ">>> ERROR: no apks built for any arch" >&2
  exit 1
fi
echo ">>> built apks for $total arch(es)"
cp /home/builder/.abuild/mtroamd-apk.rsa.pub /work/apkout/mtroamd-apk.rsa.pub
