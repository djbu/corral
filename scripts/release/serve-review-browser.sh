#!/usr/bin/env bash
set -euo pipefail

[[ "${CORRAL_E2E_BROWSER:-}" == 1 ]] || { echo 'set CORRAL_E2E_BROWSER=1 to run this opt-in fixture' >&2; exit 2; }
root="$(cd "$(dirname "$0")/../.." && pwd)"
go_mod_cache="$(go env GOMODCACHE)"; go_build_cache="$(go env GOCACHE)"
tmp="$(mktemp -d /tmp/corral-m8-browser.XXXXXX)"; tmp="$(cd "$tmp" && pwd -P)"
home="$tmp/home"; state="$home/.corral"; bin="$tmp/bin/corral"; fake="$tmp/bin/fakeclaude"; repo="$tmp/repo"
port="$(python3 - <<'PY'
import socket
s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1]);s.close()
PY
)"
proxy_port=""
if [[ "${CORRAL_E2E_BROWSER_HTTP_PROXY:-}" == 1 ]]; then
  proxy_port="$(python3 - <<'PY'
import socket
s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1]);s.close()
PY
)"
fi
export HOME="$home" GOMODCACHE="$go_mod_cache" GOCACHE="$go_build_cache"
export CORRAL_DAEMON_STATE_DIR="$state" CORRAL_DAEMON_SOCKET="$state/corral.sock" CORRAL_DAEMON_LISTEN="127.0.0.1:$port" CORRAL_DAEMON_MIN_FREE_BYTES=0
export CORRAL_SESSION_CLAUDE_BIN="$root/scripts/release/fake-review-claude.sh" CORRAL_SESSION_ENV_PASSTHROUGH=CORRAL_M8_FAKECLAUDE CORRAL_M8_FAKECLAUDE="$fake"
proxy_pid=""
cleanup(){ [[ -z "$proxy_pid" ]] || kill "$proxy_pid" >/dev/null 2>&1 || true; "$bin" daemon stop --grace 1s --timeout 5s >/dev/null 2>&1 || true; chmod -R u+w "$tmp" >/dev/null 2>&1 || true; find "$tmp" -depth -delete >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
mkdir -p "$tmp/bin" "$home" "$repo"
if [[ "${CORRAL_E2E_TRUSTED_TLS:-}" == 1 ]]; then
  command -v mkcert >/dev/null || { echo 'mkcert is required for trusted local browser TLS' >&2; exit 1; }
  mkdir -p "$tmp/tls"
  mkcert -cert-file "$tmp/tls/cert.pem" -key-file "$tmp/tls/key.pem" localhost 127.0.0.1 ::1 >/dev/null
  export CORRAL_DAEMON_TLS_CERT="$tmp/tls/cert.pem" CORRAL_DAEMON_TLS_KEY="$tmp/tls/key.pem"
fi
go build -trimpath -o "$bin" "$root/cmd/corral"; go build -trimpath -o "$fake" "$root/test/fakeclaude"
git -C "$repo" init -q -b main; git -C "$repo" config user.name 'corral M8 browser'; git -C "$repo" config user.email 'm8-browser@invalid'; printf 'base\n' > "$repo/base.txt"; git -C "$repo" add base.txt; git -C "$repo" commit -q -m base
"$bin" daemon
token="$($bin token create --label m8-browser 2>/dev/null)"
tls_cert="${CORRAL_DAEMON_TLS_CERT:-$state/tls/cert.pem}"
curl --fail --silent --show-error --cacert "$tls_cert" -H "Authorization: Bearer $token" -H 'Corral-Api-Version: 1' "https://127.0.0.1:$port/v1/dashboard" >/dev/null
dag="$($bin run --repo "$repo" --worktree --detach 'produce browser review fixture')"
deadline=$((SECONDS+20)); while ((SECONDS<deadline)); do rows="$($bin review "$dag" 2>/dev/null || true)"; grep -q pending_review <<<"$rows" && break; sleep .2; done
grep -q pending_review <<<"${rows:-}" || { echo 'browser fixture review did not become pending' >&2; exit 1; }
if [[ -n "$proxy_port" ]]; then
  go run "$root/test/reviewproxy" -listen "127.0.0.1:$proxy_port" -upstream "https://127.0.0.1:$port" -ca "$tls_cert" >"$tmp/proxy.log" 2>&1 & proxy_pid=$!
  for _ in {1..50}; do curl --fail --silent "http://127.0.0.1:$proxy_port/" >/dev/null 2>&1 && break; sleep .1; done
  printf 'BROWSER_URL=http://127.0.0.1:%s/?token=%s\n' "$proxy_port" "$token"
  printf 'BROWSER_TRANSPORT=loopback proxy with pinned TLS upstream\n'
else
  printf 'BROWSER_URL=https://127.0.0.1:%s/?token=%s\n' "$port" "$token"
fi
printf 'REPO=%s\nDAG=%s\n' "$repo" "$dag"
while [[ -f "$state/daemon.pid" ]]; do sleep 1; done
