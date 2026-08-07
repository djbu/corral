#!/usr/bin/env bash
# 01-roundtrip.sh — basic resume round-trip spike.
#
# 1. Start a fresh session, ask claude to remember a number.
# 2. Resume that session by session_id, ask it to recall the number.
# 3. Assert the recalled answer contains "42".
#
# All claude invocations run with cwd = spike/resume/sandbox so their
# session files land under a distinct ~/.claude/projects/<slug> entry.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SANDBOX="$SCRIPT_DIR/sandbox"
OUT="$SCRIPT_DIR/out"
CLAUDE="$HOME/.local/bin/claude"

mkdir -p "$SANDBOX" "$OUT"

echo "=== 01-roundtrip.sh ==="

echo "-> turn 1: teach the number"
RAW1=$(cd "$SANDBOX" && "$CLAUDE" -p "Remember the number 42. Reply only: OK" --model haiku --output-format json)
echo "$RAW1" > "$OUT/01-turn1.json"

SESSION_ID=$(echo "$RAW1" | jq -r '.session_id')
if [[ -z "$SESSION_ID" || "$SESSION_ID" == "null" ]]; then
  echo "FAIL: could not extract session_id from turn 1 output"
  echo "$RAW1"
  exit 1
fi
echo "   session_id=$SESSION_ID"

echo "-> turn 2: resume and ask for recall"
RAW2=$(cd "$SANDBOX" && "$CLAUDE" -p --resume "$SESSION_ID" "What number did I tell you to remember? Reply with only the number." --model haiku --output-format json)
echo "$RAW2" > "$OUT/01-turn2.json"

RESULT=$(echo "$RAW2" | jq -r '.result')
RESUMED_SESSION_ID=$(echo "$RAW2" | jq -r '.session_id')

echo "   turn2 result: $RESULT"
echo "   turn2 session_id: $RESUMED_SESSION_ID"

if [[ "$RESULT" == *"42"* ]]; then
  echo "PASS: resumed session recalled 42"
else
  echo "FAIL: resumed session did not recall 42 (got: $RESULT)"
  exit 1
fi

if [[ "$SESSION_ID" == "$RESUMED_SESSION_ID" ]]; then
  echo "NOTE: session_id UNCHANGED across resume ($SESSION_ID)"
else
  echo "NOTE: session_id CHANGED across resume (was $SESSION_ID, now $RESUMED_SESSION_ID)"
fi

exit 0
