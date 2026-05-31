#!/usr/bin/env bash
#
# provision-apt-gpg-key.sh — one-time setup of the apt-repo signing key.
#
# Generates a passphraseless ed25519 GPG signing key, stores the PRIVATE key
# in the mtroamd repo's Actions secret APT_GPG_KEY (piped straight to gh —
# never printed), and exports the PUBLIC key for gh-pages + the repo docs.
#
# The private key never touches disk or the terminal. The public key is safe
# to share/commit. Run it from anywhere; it writes the public exports into the
# mtroamd repo it can locate (or $PWD).
#
# Prereqs: gpg, gh (authenticated: `gh auth login`).
# Usage:   scripts/provision-apt-gpg-key.sh [--yes]
set -euo pipefail

KEY_UID="mtroamd apt repo"
REPO="AG-Studio-Apps/mtroamd"
ASSUME_YES="${1:-}"

note() { printf '\033[1;36m▸ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m✘ %s\033[0m\n' "$*" >&2; exit 1; }

confirm() {
	[ "$ASSUME_YES" = "--yes" ] && return 0
	printf '%s [y/N] ' "$1"; read -r ans
	case "$ans" in [yY]*) return 0 ;; *) return 1 ;; esac
}

# --- preconditions ----------------------------------------------------------
command -v gpg >/dev/null || die "gpg not found"
command -v gh  >/dev/null || die "gh (GitHub CLI) not found — install it and run 'gh auth login'"
gh auth status >/dev/null 2>&1 || die "gh is not authenticated — run 'gh auth login'"

# Locate the mtroamd repo for the public-key exports (fall back to $PWD).
REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || pwd)"
note "public-key exports will be written under: $REPO_ROOT"

# --- generate (or reuse) the signing key ------------------------------------
if gpg --list-secret-keys "$KEY_UID" >/dev/null 2>&1; then
	warn "a secret key for \"$KEY_UID\" already exists in your keyring."
	confirm "Reuse it (instead of generating a new one)?" \
		|| die "Aborting so you don't end up with two signing keys. Delete the existing one first if you want a fresh key."
else
	note "generating passphraseless ed25519 signing key \"$KEY_UID\""
	# --pinentry-mode loopback + empty --passphrase forces an UNprotected key
	# without a pinentry prompt; `sign never` is only usage+expiry, not a
	# passphrase setting, so without these gpg 2.x still prompts to encrypt it.
	gpg --batch --pinentry-mode loopback --passphrase '' \
		--quick-generate-key "$KEY_UID" ed25519 sign never
fi

# Resolve the full fingerprint (precise; avoids UID ambiguity).
FPR="$(gpg --list-secret-keys --with-colons "$KEY_UID" | awk -F: '/^fpr:/{print $10; exit}')"
[ -n "$FPR" ] || die "could not resolve key fingerprint"
note "key fingerprint: $FPR"

# --- store the PRIVATE key as the Actions secret (never printed) ------------
if gh secret list --repo "$REPO" 2>/dev/null | grep -q '^APT_GPG_KEY'; then
	confirm "APT_GPG_KEY already set on $REPO — overwrite it?" \
		|| die "Aborting; existing secret left untouched."
fi
note "setting APT_GPG_KEY secret on $REPO (key piped to gh, not displayed)"
gpg --armor --export-secret-keys "$FPR" | gh secret set APT_GPG_KEY --repo "$REPO"
note "passphraseless key → leave APT_GPG_PASSPHRASE UNSET."

# --- export the PUBLIC key --------------------------------------------------
KEYRING="$REPO_ROOT/mtroamd-archive-keyring.gpg"   # dearmored — goes on gh-pages root
DOC_GPG="$REPO_ROOT/docs/apt-signing-key.gpg"
DOC_ASC="$REPO_ROOT/docs/apt-signing-key.asc"
mkdir -p "$REPO_ROOT/docs"
gpg --export "$FPR"        > "$KEYRING"
gpg --export "$FPR"        > "$DOC_GPG"
gpg --armor --export "$FPR" > "$DOC_ASC"
note "public key exported:"
printf '    %s   (copy to the gh-pages branch root)\n' "$KEYRING"
printf '    %s\n    %s   (commit on feature/apt-packaging)\n' "$DOC_GPG" "$DOC_ASC"

cat <<EOF

✓ Done. Next (see docs/apt-repo-setup.md):
  1. Commit docs/apt-signing-key.{gpg,asc} on feature/apt-packaging.
  2. Seed the gh-pages branch (conf/distributions, .nojekyll, and the
     mtroamd-archive-keyring.gpg above), then enable Pages → gh-pages.
  3. Push the branch + cut a -rc tag → it publishes to the 'dev' suite.
EOF
