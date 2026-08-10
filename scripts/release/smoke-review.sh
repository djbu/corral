#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
go_mod_cache="$(go env GOMODCACHE)"
go_build_cache="$(go env GOCACHE)"
tmp="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/corral-m8.XXXXXX")"
tmp="$(cd "$tmp" && pwd -P)"
home="$tmp/home"
state="$home/.corral"
bin="$tmp/bin/corral"
fake="$tmp/bin/fakeclaude"

export HOME="$home"
export GOMODCACHE="$go_mod_cache"
export GOCACHE="$go_build_cache"
export CORRAL_DAEMON_STATE_DIR="$state"
export CORRAL_DAEMON_SOCKET="$state/corral.sock"
export CORRAL_DAEMON_MIN_FREE_BYTES=0
export CORRAL_DAEMON_SHUTDOWN_GRACE=2s
export CORRAL_SESSION_CLAUDE_BIN="$root/scripts/release/fake-review-claude.sh"
export CORRAL_SESSION_ENV_PASSTHROUGH=CORRAL_M8_FAKECLAUDE
export CORRAL_M8_FAKECLAUDE="$fake"

cleanup(){ "$bin" daemon stop --grace 1s --timeout 5s >/dev/null 2>&1 || true; rm -rf -- "$tmp"; }
trap cleanup EXIT
mkdir -p "$tmp/bin" "$home"
go build -trimpath -o "$bin" "$root/cmd/corral"
go build -trimpath -o "$fake" "$root/test/fakeclaude"

new_repo(){
  local repo="$1"
  mkdir -p "$repo"
  git -C "$repo" init -q -b main
  git -C "$repo" config user.name 'corral M8 smoke'
  git -C "$repo" config user.email 'm8-smoke@invalid'
  printf 'base\n' > "$repo/base.txt"
  git -C "$repo" add base.txt
  git -C "$repo" commit -q -m base
}

wait_review(){
  local dag="$1" deadline=$((SECONDS+20)) out
  while ((SECONDS<deadline)); do
    out="$($bin review "$dag" 2>/dev/null || true)"
    if printf '%s\n' "$out" | grep -q 'pending_review'; then printf '%s\n' "$out"; return 0; fi
    sleep .2
  done
  echo "M8 smoke: review did not become pending" >&2; return 1
}

task_id(){ awk 'NR==2 {print $1}' <<<"$1"; }
json_field(){ JSON="$1" KEY="$2" python3 - <<'PY'
import json,os
print(json.loads(os.environ['JSON'])[os.environ['KEY']])
PY
}

"$bin" daemon

release_repo="$tmp/release-repo"; new_repo "$release_repo"
release_dag="$($bin run --repo "$release_repo" --worktree --detach 'produce review release fixture')"
release_rows="$(wait_review "$release_dag")"; release_task="$(task_id "$release_rows")"
[[ ! -e "$release_repo/m8-review.txt" ]] || { echo 'task escaped worktree before review' >&2; exit 1; }
release_preflight="$($bin review preflight "$release_task" --strategy merge --target main --json)"
release_head="$(json_field "$release_preflight" target_head)"
"$bin" review release "$release_task" --strategy merge --target main --expect "$release_head"
grep -q 'change produced' "$release_repo/m8-review.txt"

discard_repo="$tmp/discard-repo"; new_repo "$discard_repo"
discard_dag="$($bin run --repo "$discard_repo" --name repeated --worktree --detach 'produce review discard fixture')"
discard_rows="$(wait_review "$discard_dag")"; discard_task="$(task_id "$discard_rows")"
"$bin" daemon restart --grace 1s --timeout 10s
discard_preflight="$($bin review preflight "$discard_task" --strategy branch --json)"
discard_head="$(json_field "$discard_preflight" task_head)"
"$bin" review discard "$discard_task" --expect "$discard_head"
[[ ! -e "$discard_repo/m8-review.txt" ]]
git -C "$discard_repo" for-each-ref --format='%(refname)' refs/corral/recovery/ | grep -q "refs/corral/recovery/$discard_task/"

# A retained task branch must not collide with an identically named task in a
# later DAG for the same repository.
repeat_dag="$($bin run --repo "$discard_repo" --name repeated --worktree --detach 'repeat the task name in a new DAG')"
repeat_rows="$(wait_review "$repeat_dag")"; repeat_task="$(task_id "$repeat_rows")"
repeat_preflight="$($bin review preflight "$repeat_task" --strategy branch --json)"
repeat_head="$(json_field "$repeat_preflight" task_head)"
"$bin" review discard "$repeat_task" --expect "$repeat_head"

conflict_repo="$tmp/conflict-repo"; new_repo "$conflict_repo"
printf 'main value\n' > "$conflict_repo/m8-review.txt"
git -C "$conflict_repo" add m8-review.txt
git -C "$conflict_repo" commit -q -m 'main conflict seed'
conflict_dag="$($bin run --repo "$conflict_repo" --worktree --detach 'produce conflicting review fixture')"
conflict_rows="$(wait_review "$conflict_dag")"; conflict_task="$(task_id "$conflict_rows")"
printf 'main advanced differently\n' > "$conflict_repo/m8-review.txt"
git -C "$conflict_repo" add m8-review.txt
git -C "$conflict_repo" commit -q -m 'advance main into conflict'
before="$(git -C "$conflict_repo" rev-parse HEAD)"
set +e
conflict_output="$($bin review preflight "$conflict_task" --strategy merge --target main --json 2>&1)"; conflict_status=$?
set -e
[[ $conflict_status -ne 0 ]] && grep -q 'task conflicts with target' <<<"$conflict_output"
[[ "$(git -C "$conflict_repo" rev-parse HEAD)" == "$before" ]]

echo 'M8 review smoke: release, restart recovery, recoverable discard and conflict gate passed'
