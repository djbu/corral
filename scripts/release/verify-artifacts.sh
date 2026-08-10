#!/usr/bin/env bash
set -euo pipefail

dist="${1:-}"
version="${2:-}"
commit="${3:-}"

if [[ -z "$dist" || -z "$version" || ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo "usage: verify-artifacts.sh <dist> <version> <40-char-commit>" >&2
  exit 2
fi

dist="$(cd "$dist" && pwd)"
checksum_file="$dist/corral_${version}_checksums.txt"
targets=(darwin_amd64 darwin_arm64 linux_amd64 linux_arm64)

if [[ ! -f "$checksum_file" ]]; then
  echo "release: missing checksum file: $checksum_file" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$dist" && sha256sum --check "$(basename "$checksum_file")")
else
  (cd "$dist" && shasum -a 256 --check "$(basename "$checksum_file")")
fi

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) native=darwin_arm64 ;;
  Darwin/x86_64) native=darwin_amd64 ;;
  Linux/aarch64|Linux/arm64) native=linux_arm64 ;;
  Linux/x86_64) native=linux_amd64 ;;
  *) native=none ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for target in "${targets[@]}"; do
  archive="corral_${version}_${target}.tar.gz"
  archive_path="$dist/$archive"
  sbom_path="$dist/$archive.sbom.json"

  if [[ ! -f "$archive_path" ]]; then
    echo "release: missing archive: $archive" >&2
    exit 1
  fi
  if [[ ! -f "$sbom_path" ]]; then
    echo "release: missing SBOM: $(basename "$sbom_path")" >&2
    exit 1
  fi

  listing="$tmp/$target.list"
  tar -tzf "$archive_path" >"$listing"
  if awk -F/ '$1 == "" || $0 ~ /(^|\/)\.\.($|\/)/ { bad=1 } END { exit !bad }' "$listing"; then
    echo "release: unsafe path in $archive" >&2
    exit 1
  fi

  normalized="$tmp/$target.normalized"
  sed -e 's#^\./##' -e '/\/$/d' "$listing" | LC_ALL=C sort >"$normalized"
  if ! diff -u <(printf '%s\n' LICENSE README.md corral | LC_ALL=C sort) "$normalized"; then
    echo "release: unexpected inventory in $archive" >&2
    exit 1
  fi

  extract="$tmp/$target"
  mkdir -p "$extract"
  tar -xzf "$archive_path" -C "$extract"
  expected="corral $version ($commit, api 1)"
  metadata="v1|$version|$commit|1"

  if [[ "$target" == "$native" ]]; then
    actual="$($extract/corral --version)"
    if [[ "$actual" != "$expected" ]]; then
      echo "release: $archive reports '$actual', expected '$expected'" >&2
      exit 1
    fi
  elif ! strings "$extract/corral" | grep -Fx "$metadata" >/dev/null; then
    echo "release: $archive does not contain expected release metadata" >&2
    exit 1
  fi

  python3 - "$sbom_path" "$archive" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
archive = sys.argv[2]
document = json.loads(path.read_text())
if document.get("spdxVersion") != "SPDX-2.3":
    raise SystemExit(f"release: {path.name} is not an SPDX 2.3 document")
if archive not in json.dumps(document, sort_keys=True):
    raise SystemExit(f"release: {path.name} does not reference {archive}")
PY
done

archive_count="$(find "$dist" -maxdepth 1 -type f -name "corral_${version}_*.tar.gz" | wc -l | tr -d ' ')"
sbom_count="$(find "$dist" -maxdepth 1 -type f -name "corral_${version}_*.tar.gz.sbom.json" | wc -l | tr -d ' ')"
if [[ "$archive_count" != "4" || "$sbom_count" != "4" ]]; then
  echo "release: expected four archives and four SBOMs, found $archive_count and $sbom_count" >&2
  exit 1
fi

echo "release: verified four archives, checksums, SBOMs and version metadata"
