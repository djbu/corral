#!/usr/bin/env bash
# Spawns the pinned 2.1.224 claude binary headless (-p) with an explicit env
# allowlist (never inherit this shell's CLAUDE_CODE_CHILD_SESSION etc, per
# spike/verify/NOTES.md's finding #3 replicated). Injects CORRAL_HOOKCAP_*
# probe vars so the hook-capture binary can prove env inheritance (V10).
#
# Usage: run_headless.sh <settings-file> <session-id-or-empty> <resume:0|1> <prompt...>
set -euo pipefail

SETTINGS="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"; SID="$2"; RESUME="$3"; shift 3
PROMPT="$*"
CLAUDE_BIN="$HOME/.local/share/claude/versions/2.1.224"
SANDBOX="/Users/danielbecerra/code/research/corral/spike/hooks/sandbox"

ARGS=(-p "$PROMPT" --model haiku --output-format json --permission-mode bypassPermissions --settings "$SETTINGS")
if [ "$RESUME" = "1" ]; then
  ARGS+=(--resume "$SID")
elif [ -n "$SID" ]; then
  ARGS+=(--session-id "$SID")
fi

cd "$SANDBOX"
env -i \
  HOME="$HOME" USER="$USER" LOGNAME="$LOGNAME" SHELL="$SHELL" PATH="$PATH" \
  TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-en_US.UTF-8}" \
  ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}" ANTHROPIC_AUTH_TOKEN="${ANTHROPIC_AUTH_TOKEN:-}" \
  ANTHROPIC_BASE_URL="${ANTHROPIC_BASE_URL:-}" ANTHROPIC_CUSTOM_HEADERS="${ANTHROPIC_CUSTOM_HEADERS:-}" \
  TERM=xterm-256color \
  CORRAL_HOOKCAP_TESTVAR="v10-env-inherit-marker-$$" \
  CORRAL_SESSION_ID_FAKE="fake-sock-marker-$$" \
  "$CLAUDE_BIN" "${ARGS[@]}"
