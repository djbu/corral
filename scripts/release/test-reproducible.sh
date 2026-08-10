#!/usr/bin/env bash
set -euo pipefail

tag="${1:-}"
goreleaser_bin="${GORELEASER_BIN:-goreleaser}"

if [[ -z "$tag" ]]; then
  echo "usage: test-reproducible.sh <annotated-tag>" >&2
  exit 2
fi

scripts/release/verify-tag.sh "$tag"
version="${tag#v}"
commit="$(git rev-parse "refs/tags/$tag^{}")"
root="$(git rev-parse --show-toplevel)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for run in one two; do
  repo="$tmp/$run"
  git clone --quiet --no-hardlinks "$root" "$repo"
  git -C "$repo" checkout --quiet --detach "$tag"
  dist="$repo/dist"
  (cd "$repo" && "$goreleaser_bin" release --clean --skip=publish,announce)
  "$repo/scripts/release/verify-artifacts.sh" "$dist" "$version" "$commit"
  find "$dist" -maxdepth 1 -type f -name "corral_${version}_*.tar.gz" -exec shasum -a 256 {} \; \
    | sed "s#$dist/##" | LC_ALL=C sort >"$tmp/$run.sha256"
done

diff -u "$tmp/one.sha256" "$tmp/two.sha256"
echo "release: two clean builds produced identical archive SHA-256 values"
