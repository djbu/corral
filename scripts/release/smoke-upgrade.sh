#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
tag="${1:-}"
[[ "$tag" =~ ^v0\.7\.0-rc\.[0-9]+$ ]] || {
  echo "usage: smoke-upgrade.sh <v0.7.0-rc.N>" >&2
  exit 2
}
command -v gh >/dev/null || { echo "smoke: gh is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "smoke: python3 is required" >&2; exit 1; }
[[ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]] || { echo "smoke: GH_TOKEN is required" >&2; exit 1; }
go_mod_cache="$(go env GOMODCACHE)"
go_build_cache="$(go env GOCACHE)"

tmp="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/corral-m7f.XXXXXX")"
tmp="$(cd "$tmp" && pwd -P)"
old_tree="$tmp/v0.6.0"
prefix="$tmp/prefix"
smoke_home="$tmp/home"
state="$smoke_home/.corral"
sock="$state/corral.sock"
fake_home="$smoke_home"
fake_state="$tmp/fake-state"
workspace="$tmp/workspace"
backup="$tmp/pre-restore.db"
bin="$prefix/bin/corral"

export HOME="$smoke_home"
export GOMODCACHE="$go_mod_cache"
export GOCACHE="$go_build_cache"
export CORRAL_DAEMON_STATE_DIR="$state"
export CORRAL_DAEMON_SOCKET="$sock"
export CORRAL_DAEMON_SHUTDOWN_GRACE=2s
export CORRAL_DAEMON_MIN_FREE_BYTES=0
export CORRAL_SESSION_CLAUDE_BIN="$prefix/bin/fakeclaude"
export CORRAL_SESSION_ENV_PASSTHROUGH=CORRAL_FAKE_HOME,CORRAL_FAKE_STATE
export CORRAL_FAKE_HOME="$fake_home"
export CORRAL_FAKE_STATE="$fake_state"

cleanup() {
  if [[ -x "$bin" ]]; then
    "$bin" daemon stop --grace 1s --timeout 5s >/dev/null 2>&1 || true
  fi
  if [[ -f "$state/daemon.pid" ]]; then
    daemon_pid="$(tr -dc '0-9' <"$state/daemon.pid")"
    [[ -z "$daemon_pid" ]] || kill "$daemon_pid" >/dev/null 2>&1 || true
  fi
  git -C "$root" worktree remove --force "$old_tree" >/dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT

step() { printf '\n==> %s\n' "$*"; }

wait_for_session() {
  local wanted="$1" deadline=$((SECONDS + 20)) output
  while ((SECONDS < deadline)); do
    if output="$($bin ls --json 2>/dev/null)" && SESSION_JSON="$output" WANTED="$wanted" python3 - <<'PY'
import json, os, sys
rows = json.loads(os.environ["SESSION_JSON"])
sys.exit(0 if any(row.get("name") == "upgrade-smoke" and row.get("status") == os.environ["WANTED"] for row in rows) else 1)
PY
    then
      return 0
    fi
    sleep 0.2
  done
  "$bin" ls --json >&2 || true
  echo "smoke: session did not reach $wanted" >&2
  return 1
}

invocation_count() {
  find "$fake_state" -name 'invocation-*.json' -type f 2>/dev/null | wc -l | tr -d ' '
}

wait_for_invocations() {
  local wanted="$1" deadline=$((SECONDS + 20))
  while ((SECONDS < deadline)); do
    [[ "$(invocation_count)" -ge "$wanted" ]] && return 0
    sleep 0.2
  done
  echo "smoke: expected at least $wanted fakeclaude invocations, got $(invocation_count)" >&2
  return 1
}

mkdir -p "$prefix/bin" "$smoke_home" "$fake_home" "$fake_state" "$workspace"

step "build and install the real v0.6.0 baseline"
git -C "$root" worktree add --detach "$old_tree" v0.6.0
old_commit="$(git -C "$old_tree" rev-parse HEAD)"
if [[ "$(uname -s)" == Linux ]]; then
  # v0.6.0 used the Darwin-only syscall.Getsid symbol. Apply exactly the
  # portability fix later committed as 6d81260; no behavior or schema changes.
  perl -0pi -e 's/\t"syscall"\n/\t"golang.org\/x\/sys\/unix"\n/; s/syscall\.Getsid/unix.Getsid/g' \
    "$old_tree/internal/daemon/daemon.go"
  grep -q 'unix.Getsid(0)' "$old_tree/internal/daemon/daemon.go"
fi
(
  cd "$old_tree"
  go build -trimpath -ldflags "-X github.com/danielbecerra/corral/internal/version.Version=0.6.0 -X github.com/danielbecerra/corral/internal/version.Commit=$old_commit" -o "$bin" ./cmd/corral
  go build -trimpath -o "$prefix/bin/fakeclaude" ./test/fakeclaude
)
"$bin" --version | grep -F "corral 0.6.0 ($old_commit, api 1)"

step "create live v0.6.0 state and session"
"$bin" daemon
new_output="$($bin new --cwd "$workspace" --name upgrade-smoke --no-attach)"
printf '%s\n' "$new_output"
session_id="$(printf '%s\n' "$new_output" | sed -n 's/^created upgrade-smoke (\([^)]*\))$/\1/p')"
[[ -n "$session_id" ]] || { echo "smoke: could not parse v0.6.0 session id" >&2; exit 1; }
wait_for_session running
wait_for_invocations 1
# Safe recovery requires a transcript. Use the same minimal JSONL fixture as
# v0.6.0's lifecycle E2E; attach itself is exercised against the upgraded RC.
SMOKE_HOME="$smoke_home" SMOKE_CWD="$workspace" SESSION_ID="$session_id" python3 - <<'PY'
import json, os, pathlib
slug = os.path.realpath(os.environ["SMOKE_CWD"]).replace("/", "-")
path = pathlib.Path(os.environ["SMOKE_HOME"]) / ".claude" / "projects" / slug / (os.environ["SESSION_ID"] + ".jsonl")
path.parent.mkdir(parents=True, exist_ok=True)
path.write_text(json.dumps({"text": "upgrade seed", "at": "2026-08-10T00:00:00Z"}) + "\n")
print(f"baseline fixture: wrote resumable transcript {path}")
PY
schema_before="$(DB_PATH="$state/corral.db" python3 - <<'PY'
import os, sqlite3
with sqlite3.connect(os.environ["DB_PATH"]) as db:
    print(db.execute("PRAGMA user_version").fetchone()[0])
PY
)"
[[ "$schema_before" == 6 ]] || { echo "smoke: v0.6.0 fixture has unexpected schema $schema_before" >&2; exit 1; }

step "upgrade in place from the authenticated private RC"
if [[ -n "${CORRAL_SMOKE_RELEASE_DIR:-}" ]]; then
  asset="corral_${tag#v}_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m).tar.gz"
  [[ "$(uname -m)" != x86_64 ]] || asset="${asset%_x86_64.tar.gz}_amd64.tar.gz"
  [[ "$(uname -m)" != aarch64 ]] || asset="${asset%_aarch64.tar.gz}_arm64.tar.gz"
  "$root/scripts/install.sh" --version "${tag#v}" --prefix "$prefix" \
    --archive "$CORRAL_SMOKE_RELEASE_DIR/$asset" \
    --checksums "$CORRAL_SMOKE_RELEASE_DIR/corral_${tag#v}_checksums.txt"
else
  "$root/scripts/install.sh" --version "${tag#v}" --prefix "$prefix"
fi
"$bin" --version | grep -E "^corral ${tag#v} \([0-9a-f]{40}, api 1\)$"

step "new CLI gracefully stops the old daemon, migrates, and recovers"
"$bin" daemon stop --grace 2s --timeout 20s
"$bin" daemon start
wait_for_session running
wait_for_invocations 2
DB_PATH="$state/corral.db" python3 - <<'PY'
import os, sqlite3
with sqlite3.connect(os.environ["DB_PATH"]) as db:
    version = db.execute("PRAGMA user_version").fetchone()[0]
    assert version == 6, f"expected compatible schema 6, got {version}"
    row = db.execute("SELECT name, desired_state FROM sessions WHERE name='upgrade-smoke'").fetchone()
    assert row == ("upgrade-smoke", "running"), row
print("migration smoke: schema=6 reopened idempotently and session intent preserved")
PY

step "exercise restart, kill/wake, and attach through a real PTY"
"$bin" daemon restart --grace 2s --timeout 20s
wait_for_session running
wait_for_invocations 3
"$bin" kill --grace 2s upgrade-smoke
wait_for_session exited
"$bin" wake upgrade-smoke
wait_for_session running
wait_for_invocations 4
python3 "$root/scripts/release/pty-attach-smoke.py" "$bin" upgrade-smoke

step "hot backup, stopped restore, and post-restore restart"
"$bin" backup --output "$backup"
"$bin" daemon stop --grace 2s --timeout 20s
"$bin" restore --replace "$backup"
"$bin" daemon start
wait_for_session running

step "remote TLS and bearer-token enforcement"
"$bin" daemon stop --grace 2s --timeout 20s
port="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
export CORRAL_DAEMON_LISTEN="127.0.0.1:$port"
"$bin" daemon start
token="$($bin token create --label m7f-smoke)"
token_id="$($bin token list | awk '$2 == "m7f-smoke" {print $1}')"
[[ -n "$token" && -n "$token_id" ]]
CORRAL_CLIENT_HOST="127.0.0.1:$port" CORRAL_CLIENT_TOKEN="$token" \
CORRAL_CLIENT_CACERT="$state/tls/cert.pem" "$bin" ls --json | grep -F '"name": "upgrade-smoke"'
"$bin" token revoke "$token_id"
if CORRAL_CLIENT_HOST="127.0.0.1:$port" CORRAL_CLIENT_TOKEN="$token" \
   CORRAL_CLIENT_CACERT="$state/tls/cert.pem" "$bin" ls --json >"$tmp/revoked.out" 2>"$tmp/revoked.err"; then
  echo "smoke: revoked token was accepted" >&2
  exit 1
fi
grep -Eiq 'unauthorized|token|401' "$tmp/revoked.err"
unset CORRAL_DAEMON_LISTEN

step "uninstall binary without deleting user state"
"$bin" daemon stop --grace 2s --timeout 20s
"$root/scripts/install.sh" --uninstall --prefix "$prefix"
[[ ! -e "$bin" ]]
[[ -f "$state/corral.db" && -d "$state/sessions" ]]

if [[ "$(uname -s)" == Darwin && -z "${CORRAL_SMOKE_RELEASE_DIR:-}" && "${CORRAL_SMOKE_HOMEBREW:-1}" == 1 ]]; then
  step "install the same private RC through its checksum-pinned Homebrew formula"
  release_dir="$tmp/release"
  formula="$tmp/Formula/corral.rb"
  assets_json="$release_dir/assets.json"
  mkdir -p "$release_dir"
  GH_TOKEN="${GH_TOKEN:-$GITHUB_TOKEN}" gh release download "$tag" --repo djbu/corral \
    --dir "$release_dir" --pattern "corral_${tag#v}_checksums.txt"
  GH_TOKEN="${GH_TOKEN:-$GITHUB_TOKEN}" gh release view "$tag" --repo djbu/corral \
    --json assets >"$assets_json"
  "$root/scripts/release/render-homebrew-formula.sh" "${tag#v}" \
    "$release_dir/corral_${tag#v}_checksums.txt" "$formula" "$assets_json"
  tap=djbu/corral-smoke
  brew tap-new --no-git "$tap"
  tap_root="$(brew --repository "$tap")"
  install -m 0644 "$formula" "$tap_root/Formula/corral.rb"
  export HOMEBREW_GITHUB_API_TOKEN="${GH_TOKEN:-$GITHUB_TOKEN}"
  export HOMEBREW_NO_AUTO_UPDATE=1
  export HOMEBREW_NO_INSTALL_CLEANUP=1
  brew install "$tap/corral"
  brew test "$tap/corral"
  brew uninstall "$tap/corral"
  brew untap "$tap"
  unset HOMEBREW_GITHUB_API_TOKEN HOMEBREW_NO_AUTO_UPDATE HOMEBREW_NO_INSTALL_CLEANUP
fi

step "M7F smoke passed on $(uname -s)/$(uname -m)"
printf 'baseline=v0.6.0 rc=%s schema_before=%s schema_after=6 invocations=%s state_preserved=yes\n' \
  "$tag" "$schema_before" "$(invocation_count)"
