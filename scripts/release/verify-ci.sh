#!/usr/bin/env bash
set -euo pipefail

repo="${GITHUB_REPOSITORY:-djbu/corral}"
commit="${1:-${GITHUB_SHA:-}}"
tag="${2:-${GITHUB_REF_NAME:-}}"

if [[ -z "$commit" || -z "$tag" ]]; then
  echo "usage: verify-ci.sh <commit> <tag>" >&2
  exit 2
fi

if gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
  echo "release: $tag already has a GitHub release; versions are immutable" >&2
  exit 1
fi

checks="$(gh api "repos/$repo/commits/$commit/check-runs?per_page=100")"
for required in "test (macos-latest)" "test (ubuntu-latest)"; do
  successes="$(jq --arg name "$required" '[.check_runs[] | select(.name == $name and .status == "completed" and .conclusion == "success")] | length' <<<"$checks")"
  if [[ "$successes" -lt 1 ]]; then
    echo "release: required successful check is missing: $required" >&2
    exit 1
  fi
done

echo "release: required macOS and Ubuntu checks are green for $commit"
