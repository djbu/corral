#!/usr/bin/env bash
set -euo pipefail

tag="${1:-${GITHUB_REF_NAME:-}}"

if [[ -z "$tag" ]]; then
  echo "usage: verify-tag.sh <vX.Y.Z[-prerelease]>" >&2
  exit 2
fi

if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "release: tag is not supported SemVer: $tag" >&2
  exit 1
fi

if [[ "$(git cat-file -t "refs/tags/$tag" 2>/dev/null || true)" != "tag" ]]; then
  echo "release: $tag must be an annotated tag" >&2
  exit 1
fi

tag_commit="$(git rev-parse "refs/tags/$tag^{}")"
head_commit="$(git rev-parse HEAD)"
if [[ "$tag_commit" != "$head_commit" ]]; then
  echo "release: $tag points to $tag_commit, checkout is $head_commit" >&2
  exit 1
fi

if [[ -n "$(git status --porcelain --untracked-files=normal)" ]]; then
  echo "release: checkout is dirty" >&2
  git status --short >&2
  exit 1
fi

if [[ "$(sed -n '1p' go.mod)" != "module github.com/djbu/corral" ]]; then
  echo "release: go.mod does not declare the canonical module" >&2
  exit 1
fi

if [[ "$(tr -d '[:space:]' < .go-version)" != "1.26.5" ]]; then
  echo "release: .go-version must pin Go 1.26.5" >&2
  exit 1
fi

echo "release: annotated tag $tag resolves to $tag_commit"
