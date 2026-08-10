#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
checksums="${2:-}"
output="${3:-}"

if [[ -z "$version" || -z "$checksums" || -z "$output" ]]; then
  echo "usage: render-homebrew-formula.sh <version> <checksums.txt> <output.rb>" >&2
  exit 2
fi
version="${version#v}"
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] || {
  echo "homebrew: unsupported version: $version" >&2
  exit 1
}
[[ -f "$checksums" ]] || { echo "homebrew: checksums not found: $checksums" >&2; exit 1; }

checksum_for() {
  local asset="$1" values
  values="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print tolower($1) }' "$checksums")"
  if [[ "$(printf '%s\n' "$values" | awk 'NF { count++ } END { print count+0 }')" != "1" ]] || \
     [[ ! "$values" =~ ^[0-9a-f]{64}$ ]]; then
    echo "homebrew: expected one valid SHA-256 for $asset" >&2
    return 1
  fi
  printf '%s' "$values"
}

darwin_amd64="$(checksum_for "corral_${version}_darwin_amd64.tar.gz")"
darwin_arm64="$(checksum_for "corral_${version}_darwin_arm64.tar.gz")"
linux_amd64="$(checksum_for "corral_${version}_linux_amd64.tar.gz")"
linux_arm64="$(checksum_for "corral_${version}_linux_arm64.tar.gz")"
template="$(cd "$(dirname "$0")/../.." && pwd)/packaging/homebrew/Formula/corral.rb.in"
mkdir -p "$(dirname "$output")"
sed \
  -e "s/@VERSION@/$version/g" \
  -e "s/@DARWIN_AMD64@/$darwin_amd64/g" \
  -e "s/@DARWIN_ARM64@/$darwin_arm64/g" \
  -e "s/@LINUX_AMD64@/$linux_amd64/g" \
  -e "s/@LINUX_ARM64@/$linux_arm64/g" \
  "$template" >"$output"

echo "homebrew: rendered $output for corral $version"
