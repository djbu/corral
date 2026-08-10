#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/corral-formula-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
version=0.7.0-rc.1
checksums="$tmp/checksums.txt"
value=1
for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  printf '%064x  corral_%s_%s.tar.gz\n' "$value" "$version" "$target" >>"$checksums"
  value=$((value + 1))
done

formula="$tmp/Formula/corral.rb"
"$root/scripts/release/render-homebrew-formula.sh" "$version" "$checksums" "$formula"
if command -v ruby >/dev/null 2>&1; then
  ruby -c "$formula" >/dev/null
fi
if command -v brew >/dev/null 2>&1; then
  brew style "$formula"
fi
! grep -q '@[A-Z_]*@' "$formula"
grep -q "version \"$version\"" "$formula"
grep -q 'HOMEBREW_GITHUB_API_TOKEN is required' "$formula"
[[ "$(grep -c 'sha256 "' "$formula")" == "4" ]]
[[ "$(grep -c 'releases/download/v' "$formula")" == "4" ]]

echo "homebrew test: private formula rendered with four target checksums"
