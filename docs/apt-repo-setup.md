# apt repo — one-time maintainer setup

The apt repo is served from the **`gh-pages`** branch at
`https://ag-studio-apps.github.io/meshtermd` (two suites: `stable`, `dev`).
The release workflow's `apt-repo` job publishes to it automatically, but only
once these one-time steps are done. **Do not enable `stable` publishing until
App Store 2.0 is live** — the `dev` suite can go live first for testing.

## 1. Generate the archive-signing GPG key

Separate from the minisign release key. A **passphraseless** key is recommended
(it's already protected as a CI secret, and keeps the CI signing path simple):

```sh
gpg --batch --quick-generate-key "meshtermd apt repo" ed25519 sign never

# Private key → GitHub Actions secret APT_GPG_KEY (Settings → Secrets → Actions)
gpg --armor --export-secret-keys "meshtermd apt repo"      # paste into APT_GPG_KEY
#   Leave APT_GPG_PASSPHRASE unset for a passphraseless key. If you DO use a
#   passphrase, set APT_GPG_PASSPHRASE too — CI presets it via gpg loopback.

# Public keyring (dearmored) — this is what apt clients trust:
gpg --export "meshtermd apt repo" > meshtermd-archive-keyring.gpg
```

Also commit the public key to the repo for transparency:

```sh
gpg --export "meshtermd apt repo" > docs/apt-signing-key.gpg
gpg --armor --export "meshtermd apt repo" > docs/apt-signing-key.asc
```

## 2. Seed the gh-pages branch

```sh
git switch --orphan gh-pages
git rm -rf . 2>/dev/null || true
mkdir conf
git show main:packaging/apt/conf/distributions > conf/distributions
touch .nojekyll
cp /path/to/meshtermd-archive-keyring.gpg .      # served at the Pages root
git add -A && git commit -m "seed apt repo" && git push -u origin gh-pages
git switch -   # back to your working branch
```

(The `apt-repo` CI job also copies `conf/distributions` in if missing, and runs
`reprepro includedeb` to create `dists/` + `pool/` on the first publish.)

## 3. Enable GitHub Pages

Repo **Settings → Pages → Source: Deploy from a branch → `gh-pages` / root**.
Confirm `https://ag-studio-apps.github.io/meshtermd/meshtermd-archive-keyring.gpg`
serves the key.

## 4. Verify (dev suite first)

Tag a develop build (e.g. `v2.0.0-rc.1`) → the `apt-repo` job routes it to `dev`.
On a throwaway Debian/Ubuntu box, follow `docs/apt-dev-channel.md`, then
`sudo apt update` (this verifies the GPG `Release` signature) and
`sudo apt install meshtermd`. Once 2.0 ships, a clean `vX.Y.Z` tag on `main`
publishes to `stable`.

## How routing works (no manual action)

The `apt-repo` job publishes to **`stable`** only when the tagged commit is an
ancestor of `origin/main` **and** the tag is clean semver (no `-`/prerelease);
everything else goes to **`dev`**. So production (main / App Store) and
development (develop / TestFlight) channels stay separate automatically.
