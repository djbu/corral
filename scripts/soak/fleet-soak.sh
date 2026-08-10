#!/usr/bin/env bash
# M9 step 61's opt-in fleet soak. It is intentionally a nightly job rather
# than a push gate: it builds race-instrumented binaries, keeps 50 PTYs live,
# and performs 1,000 spawn/kill cycles with periodic restart/resume recovery.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
go_mod_cache="$(go env GOMODCACHE)"
go_build_cache="$(go env GOCACHE)"
tmp="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/corral-m9-soak.XXXXXX")"
tmp="$(cd "$tmp" && pwd -P)"
home="$tmp/home"
state="$home/.corral"
bin="$tmp/bin/corral"
fake="$tmp/bin/fakeclaude"
cycles="${CORRAL_SOAK_CYCLES:-1000}"
concurrent="${CORRAL_SOAK_CONCURRENT:-50}"
restart_every="${CORRAL_SOAK_RESTART_EVERY:-100}"

export HOME="$home"
export GOMODCACHE="$go_mod_cache"
export GOCACHE="$go_build_cache"
export CORRAL_DAEMON_STATE_DIR="$state"
export CORRAL_DAEMON_SOCKET="$state/corral.sock"
export CORRAL_DAEMON_MIN_FREE_BYTES=0
export CORRAL_DAEMON_MAX_INTERACTIVE_SESSIONS="$concurrent"
export CORRAL_DAEMON_SHUTDOWN_GRACE=200ms
export CORRAL_SESSION_CLAUDE_BIN="$fake"
export CORRAL_SOAK_METRICS=1
export CORRAL_SOAK_PROFILES=1
export CORRAL_FAKE_HOME="$home"
export CORRAL_FAKE_STATE="$tmp/fake-state"

cleanup() {
  local status=$?
  "$bin" daemon stop --grace 100ms --timeout 10s >/dev/null 2>&1 || true
  if (( status == 0 )); then
    rm -rf -- "$tmp"
  else
    artifact_root="${GITHUB_WORKSPACE:-$root}/.soak-artifacts"
    rm -rf -- "$artifact_root"
    mkdir -p "$artifact_root"
    cp -R "$state/." "$artifact_root/" 2>/dev/null || true
    echo "M9 soak artifacts copied to: $artifact_root" >&2
    echo "M9 soak working directory retained: $tmp" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$tmp/bin" "$home" "$tmp/work"
go build -race -trimpath -o "$bin" "$root/cmd/corral"
go build -race -trimpath -o "$fake" "$root/test/fakeclaude"

"$bin" daemon

sample_host() {
  local label="$1" pid fd rss
  pid="$(tr -d '[:space:]' < "$state/daemon.pid")"
  if [[ -d "/proc/$pid/fd" ]]; then
    fd="$(find "/proc/$pid/fd" -mindepth 1 -maxdepth 1 -print | wc -l | tr -d '[:space:]')"
  else
    # macOS has no procfs. lsof's first line is its header, so only count
    # descriptor rows; this is the same process-level signal as /proc/fd.
    fd="$( (lsof -p "$pid" -Fn 2>/dev/null || true) | awk '/^f[0-9]+$/ { n++ } END { print n+0 }')"
  fi
  rss="$(ps -o rss= -p "$pid" | tr -d '[:space:]')"
  printf '%s\t%s\t%s\t%s\n' "$label" "$pid" "$fd" "$rss" >> "$state/soak-host.tsv"
}

new_session() {
  "$bin" new --cwd "$tmp/work" --name "$1" --no-attach >/dev/null
}

kill_session() {
  "$bin" kill --grace 100ms "$1" >/dev/null
}

sample_host baseline
for n in $(seq 1 "$concurrent"); do new_session "fleet-$n" & done
wait
sample_host fleet_live
for n in $(seq 1 "$concurrent"); do kill_session "fleet-$n" & done
wait

for n in $(seq 1 "$cycles"); do
  name="cycle-$n"
  new_session "$name"
  # Restart while the child is live. This exercises durable recovery/resume;
  # the subsequent kill returns the daemon to its quiescent baseline.
  if (( n % restart_every == 0 )); then "$bin" daemon restart --grace 100ms --timeout 20s >/dev/null; fi
  kill_session "$name"
done
sleep 2
sample_host quiescent

python3 - "$state" <<'PY'
import json, pathlib, sys
state = pathlib.Path(sys.argv[1])
rows = [line.split() for line in (state / "soak-host.tsv").read_text().splitlines()]
by_label = {row[0]: row for row in rows}
base, final = by_label["baseline"], by_label["quiescent"]
fd_delta = int(final[2]) - int(base[2])
rss_delta_kib = int(final[3]) - int(base[3])
samples = [json.loads(x) for x in (state / "soak-daemon.jsonl").read_text().splitlines()]
goroutine_delta = samples[-1]["goroutines"] - samples[0]["goroutines"]
wal_max = max(s["wal_bytes"] for s in samples)
result = {"fd_delta": fd_delta, "rss_delta_kib": rss_delta_kib,
          "goroutine_delta": goroutine_delta, "wal_max_bytes": wal_max,
          "samples": len(samples)}
(state / "soak-summary.json").write_text(json.dumps(result, indent=2) + "\n")
print(json.dumps(result))
limits = {"fd_delta": 64, "rss_delta_kib": 128 * 1024,
          "goroutine_delta": 64, "wal_max_bytes": 64 * 1024 * 1024}
bad = {k: result[k] for k, maximum in limits.items() if result[k] > maximum}
if bad:
    raise SystemExit("M9 soak leak threshold exceeded: " + json.dumps(bad))
PY

echo "M9 fleet soak passed: $concurrent concurrent sessions, $cycles spawn/kill cycles"
