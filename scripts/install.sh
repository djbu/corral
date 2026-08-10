#!/usr/bin/env bash
set -euo pipefail

repo="djbu/corral"
version=""
prefix="${CORRAL_INSTALL_PREFIX:-${HOME:?HOME is required}/.local}"
archive=""
checksums=""
dry_run=false
uninstall=false

usage() {
  cat <<'EOF'
Install a verified private corral release.

Usage:
  install.sh --version <X.Y.Z[-prerelease]> [--prefix <path>] [--dry-run]
  install.sh --version <X.Y.Z[-prerelease]> --archive <tar.gz> \
    --checksums <checksums.txt> [--prefix <path>] [--dry-run]
  install.sh --uninstall [--prefix <path>] [--dry-run]

Online installs require gh plus CORRAL_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN.
The default prefix is $CORRAL_INSTALL_PREFIX or $HOME/.local.
EOF
}

die() {
  printf 'corral installer: %s\n' "$*" >&2
  exit 1
}

while (($#)); do
  case "$1" in
    --version)
      (($# >= 2)) || die "--version requires a value"
      version="$2"
      shift 2
      ;;
    --prefix)
      (($# >= 2)) || die "--prefix requires a value"
      prefix="$2"
      shift 2
      ;;
    --archive)
      (($# >= 2)) || die "--archive requires a value"
      archive="$2"
      shift 2
      ;;
    --checksums)
      (($# >= 2)) || die "--checksums requires a value"
      checksums="$2"
      shift 2
      ;;
    --dry-run)
      dry_run=true
      shift
      ;;
    --uninstall)
      uninstall=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1 (use --help)"
      ;;
  esac
done

[[ "$(id -u)" != "0" ]] || die "do not run this installer as root or through sudo"
[[ "$prefix" == /* ]] || die "--prefix must be an absolute path"
[[ "$prefix" != "/" ]] || die "refusing to use / as the installation prefix"
target_dir="${prefix%/}/bin"
target="$target_dir/corral"

if $uninstall; then
  [[ -z "$version" && -z "$archive" && -z "$checksums" ]] || \
    die "--uninstall cannot be combined with release options"
  if $dry_run; then
    printf 'would remove %s; user data and service definitions are preserved\n' "$target"
    exit 0
  fi
  if [[ -d "$target" ]]; then
    die "refusing to remove directory at $target"
  fi
  if [[ -e "$target" || -L "$target" ]]; then
    rm -f -- "$target"
    printf 'removed %s; user data and service definitions were preserved\n' "$target"
  else
    printf 'already absent: %s\n' "$target"
  fi
  exit 0
fi

[[ -n "$version" ]] || die "--version is required"
version="${version#v}"
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] || \
  die "unsupported version: $version"
[[ -n "$archive" && -n "$checksums" || -z "$archive" && -z "$checksums" ]] || \
  die "offline installation requires both --archive and --checksums"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

asset="corral_${version}_${os}_${arch}.tar.gz"
checksum_asset="corral_${version}_checksums.txt"
source_description="private GitHub release v$version"
if [[ -n "$archive" ]]; then
  source_description="offline archive $archive"
fi

printf 'corral %s (%s/%s) -> %s\n' "$version" "$os" "$arch" "$target"
printf 'source: %s\n' "$source_description"
if $dry_run && [[ -z "$archive" ]]; then
  printf 'dry-run: no download or filesystem change performed\n'
  exit 0
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/corral-install.XXXXXX")"
install_tmp=""
cleanup() {
  rm -rf -- "$tmp"
  if [[ -n "$install_tmp" ]]; then
    rm -f -- "$install_tmp"
  fi
}
trap cleanup EXIT

if [[ -z "$archive" ]]; then
  command -v gh >/dev/null 2>&1 || \
    die "gh is required for private downloads; use --archive and --checksums offline"
  token="${CORRAL_GITHUB_TOKEN:-${GH_TOKEN:-${GITHUB_TOKEN:-}}}"
  [[ -n "$token" ]] || \
    die "private download requires CORRAL_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN"
  GH_TOKEN="$token" gh release download "v$version" \
    --repo "$repo" --dir "$tmp" --pattern "$asset" --pattern "$checksum_asset" || \
    die "could not download private release v$version; verify the version, token, and repository access"
  archive="$tmp/$asset"
  checksums="$tmp/$checksum_asset"
  [[ -f "$archive" && -f "$checksums" ]] || \
    die "private release v$version is missing $asset or $checksum_asset"
else
  [[ -f "$archive" ]] || die "offline archive not found: $archive"
  [[ -f "$checksums" ]] || die "checksum file not found: $checksums"
  [[ "$(basename "$archive")" == "$asset" ]] || \
    die "archive name must be $asset for this platform and version"
fi

hashes="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$checksums")"
[[ "$(printf '%s\n' "$hashes" | awk 'NF { count++ } END { print count+0 }')" == "1" ]] || \
  die "checksum file must contain exactly one entry for $asset"
expected_hash="$(printf '%s' "$hashes" | tr '[:upper:]' '[:lower:]')"
[[ "$expected_hash" =~ ^[0-9a-f]{64}$ ]] || die "invalid SHA-256 for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  actual_hash="$(sha256sum "$archive" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual_hash="$(shasum -a 256 "$archive" | awk '{print $1}')"
else
  die "sha256sum or shasum is required"
fi
[[ "$actual_hash" == "$expected_hash" ]] || \
  die "checksum mismatch for $asset; existing installation was not changed"

listing="$tmp/archive.list"
tar -tzf "$archive" >"$listing"
if awk -F/ '$1 == "" || $0 ~ /(^|\/)\.\.($|\/)/ { bad=1 } END { exit !bad }' "$listing"; then
  die "archive contains an unsafe path"
fi
normalized="$tmp/archive.normalized"
sed -e 's#^\./##' -e '/\/$/d' "$listing" | LC_ALL=C sort >"$normalized"
if ! diff -u <(printf '%s\n' LICENSE README.md corral | LC_ALL=C sort) "$normalized" >/dev/null; then
  die "archive inventory is not the expected LICENSE, README.md, and corral"
fi

extract="$tmp/extract"
mkdir -p "$extract"
tar -xzf "$archive" -C "$extract"
for extracted_file in LICENSE README.md corral; do
  [[ -f "$extract/$extracted_file" && ! -L "$extract/$extracted_file" ]] || \
    die "archive member is not a regular file: $extracted_file"
done
chmod 0755 "$extract/corral"
actual_version="$($extract/corral --version 2>/dev/null || true)"
[[ "$actual_version" =~ ^corral\ ${version//./\.}\ \([0-9a-f]{40},\ api\ 1\)$ ]] || \
  die "archive binary reports an unexpected version: ${actual_version:-<empty>}"

if $dry_run; then
  printf 'verified %s (%s)\n' "$asset" "$actual_hash"
  printf 'dry-run: existing installation was not changed\n'
  exit 0
fi

mkdir -p "$target_dir"
install_tmp="$(mktemp "$target_dir/.corral.install.XXXXXX")"
install -m 0755 "$extract/corral" "$install_tmp"
mv -f -- "$install_tmp" "$target"
install_tmp=""
printf 'installed %s\n' "$target"
printf 'run: %s --version\n' "$target"
