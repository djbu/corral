#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
installer="$root/scripts/install.sh"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/corral-install-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) target=darwin_arm64 ;;
  Darwin/x86_64) target=darwin_amd64 ;;
  Linux/aarch64|Linux/arm64) target=linux_arm64 ;;
  Linux/x86_64) target=linux_amd64 ;;
  *) echo "install test: unsupported host" >&2; exit 1 ;;
esac

version=0.7.0-rc.1
asset="corral_${version}_${target}.tar.gz"
payload="$tmp/payload"
mkdir -p "$payload"
printf '%s\n' '#!/usr/bin/env bash' "echo 'corral $version (0123456789012345678901234567890123456789, api 1)'" >"$payload/corral"
chmod 0755 "$payload/corral"
printf 'license\n' >"$payload/LICENSE"
printf 'readme\n' >"$payload/README.md"
tar -czf "$tmp/$asset" -C "$payload" LICENSE README.md corral
if command -v sha256sum >/dev/null 2>&1; then
  hash="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
else
  hash="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
fi
printf '%s  %s\n' "$hash" "$asset" >"$tmp/checksums.txt"

prefix="$tmp/prefix"
"$installer" --version "$version" --prefix "$prefix" \
  --archive "$tmp/$asset" --checksums "$tmp/checksums.txt"
[[ "$($prefix/bin/corral --version)" == "corral $version (0123456789012345678901234567890123456789, api 1)" ]]

printf 'old binary\n' >"$prefix/bin/corral"
printf '%064d  %s\n' 0 "$asset" >"$tmp/bad-checksums.txt"
if "$installer" --version "$version" --prefix "$prefix" \
  --archive "$tmp/$asset" --checksums "$tmp/bad-checksums.txt" 2>"$tmp/bad.err"; then
  echo "install test: bad checksum unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'checksum mismatch' "$tmp/bad.err"
[[ "$(cat "$prefix/bin/corral")" == "old binary" ]]

"$installer" --version "$version" --prefix "$prefix" \
  --archive "$tmp/$asset" --checksums "$tmp/checksums.txt" --dry-run
[[ "$(cat "$prefix/bin/corral")" == "old binary" ]]

state="$tmp/state"
mkdir -p "$state"
printf 'keep\n' >"$state/corral.db"
"$installer" --uninstall --prefix "$prefix"
[[ ! -e "$prefix/bin/corral" ]]
[[ "$(cat "$state/corral.db")" == "keep" ]]

if env -u CORRAL_GITHUB_TOKEN -u GH_TOKEN -u GITHUB_TOKEN \
  "$installer" --version "$version" --prefix "$prefix" 2>"$tmp/auth.err"; then
  echo "install test: unauthenticated private install unexpectedly succeeded" >&2
  exit 1
fi
grep -Eq 'gh is required|private download requires' "$tmp/auth.err"

echo "install test: offline install, dry-run, checksum rollback, auth failure, and uninstall passed"
