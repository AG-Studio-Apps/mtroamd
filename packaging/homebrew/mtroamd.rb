# Homebrew formula for mtroamd + mtroam.
#
# This file is the staged copy. The live tap lives at
# github.com/AG-Studio-Apps/homebrew-mtroamd; copy this file into
# Formula/mtroamd.rb there after each upstream release. Users install via:
#
#     brew tap AG-Studio-Apps/mtroamd
#     brew install mtroamd
#
# Per-release maintenance:
#   1. Update `version` to the new upstream tag.
#   2. Update the per-arch `sha256` values from the published SHA256SUMS at
#      https://github.com/AG-Studio-Apps/mtroamd/releases/download/<tag>/SHA256SUMS
#   3. Copy this file into the tap repo and `git push`.
#
# We intentionally distribute pre-built binaries (no source build) so users
# don't need a Go toolchain. The minisign-signed SHA256SUMS in the release
# verifies the bytes; the `sha256` values below pin them per-arch.

class Mtroamd < Formula
  desc "Persistent terminal daemon over QUIC — mosh+tmux in one, multi-client handoff"
  homepage "https://github.com/AG-Studio-Apps/mtroamd"
  version "0.3.1"
  license "AGPL-3.0-or-later"

  on_macos do
    on_arm do
      url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v#{version}/mtroamd-darwin-arm64"
      sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
    end
    on_intel do
      url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v#{version}/mtroamd-darwin-amd64"
      sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
    end
  end

  on_linux do
    on_arm do
      if Hardware::CPU.is_64_bit?
        url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v#{version}/mtroamd-linux-arm64"
        sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
      else
        url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v#{version}/mtroamd-linux-armv7"
        sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
      end
    end
    on_intel do
      url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v#{version}/mtroamd-linux-amd64"
      sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
    end
  end

  # Companion CLI, man pages, and shell completions live as separate
  # resource downloads from the same release.
  resource "mtroam" do
    on_macos do
      on_arm do
        url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroam-darwin-arm64"
        sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
      end
      on_intel do
        url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroam-darwin-amd64"
        sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
      end
    end
    on_linux do
      on_arm do
        if Hardware::CPU.is_64_bit?
          url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroam-linux-arm64"
          sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
        else
          url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroam-linux-armv7"
          sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
        end
      end
      on_intel do
        url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroam-linux-amd64"
        sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
      end
    end
  end

  resource "manpages" do
    url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroamd.8"
    sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
  end

  resource "mtroam-manpage" do
    url "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v0.3.1/mtroam.1"
    sha256 "REPLACE_WITH_SHA256_FROM_RELEASE_MANIFEST"
  end

  def install
    # Daemon binary is the formula's primary url; rename it to the
    # canonical name regardless of platform-tagged download filename.
    bin.install Dir["mtroamd-*"].first => "mtroamd"

    resource("mtroam").stage do
      bin.install Dir["mtroam-*"].first => "mtroam"
    end

    resource("manpages").stage do
      man8.install "mtroamd.8"
    end
    resource("mtroam-manpage").stage do
      man1.install "mtroam.1"
    end

    # Shell completions. Fetched on the fly from the same release; we
    # don't pin sha256s for them because they're text and we already
    # have the binaries verified — losing the completion sha doesn't
    # widen the trust boundary in any practical way. If that posture
    # changes, hoist these into named resources like the binaries.
    %w[bash zsh fish].each do |sh|
      %w[mtroamd mtroam].each do |bin_name|
        system "curl", "-fsSL", "-o", "#{bin_name}.#{sh}",
               "https://github.com/AG-Studio-Apps/mtroamd/releases/download/v#{version}/#{bin_name}.#{sh}"
      end
    end
    bash_completion.install "mtroamd.bash" => "mtroamd"
    bash_completion.install "mtroam.bash"     => "mtroam"
    zsh_completion.install  "mtroamd.zsh"  => "_mtroamd"
    zsh_completion.install  "mtroam.zsh"      => "_mtroam"
    fish_completion.install "mtroamd.fish" => "mtroamd.fish"
    fish_completion.install "mtroam.fish"     => "mtroam.fish"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/mtroamd version")
    assert_match version.to_s, shell_output("#{bin}/mtroam version")
  end
end
