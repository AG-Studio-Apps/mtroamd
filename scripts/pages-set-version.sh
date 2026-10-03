#!/usr/bin/env bash
# Update the "Current versions" line on the gh-pages install page.
#
#   scripts/pages-set-version.sh <index.html> <stable|dev> <tag> <YYYY-MM-DD>
#
# The page carries paired comment markers per channel:
#   <!--ver:stable-->v1.8.0<!--/ver:stable-->  <!--date:stable-->2026-09-17<!--/date:stable-->
# Only the text between a pair is replaced. Exits 0 with a notice (no change) when the page
# has no markers, so an older page never fails a release. Run by release.yml's pages-version job.
set -euo pipefail

index=${1:?index.html}; suite=${2:?suite}; tag=${3:?tag}; date=${4:?date}

case "$suite" in stable|dev) ;; *) echo "bad suite: $suite" >&2; exit 2 ;; esac
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "bad tag: $tag" >&2; exit 2; }
[[ "$date" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "bad date: $date" >&2; exit 2; }

if ! grep -q "<!--ver:${suite}-->" "$index"; then
  echo "::notice::no <!--ver:${suite}--> marker in $index; version line not updated"
  exit 0
fi

sed -i -E \
  -e "s#(<!--ver:${suite}-->)[^<]*(<!--/ver:${suite}-->)#\1${tag}\2#" \
  -e "s#(<!--date:${suite}-->)[^<]*(<!--/date:${suite}-->)#\1${date}\2#" \
  "$index"
echo "page: ${suite} = ${tag} (${date})"
