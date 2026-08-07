#!/usr/bin/env bash
# 02-kill-midturn.sh — kill claude mid-turn, then attempt to resume.
#
# Starts a long-ish streaming turn in the background, waits for a handful
# of stream-json events to land on disk, SIGKILLs the process mid-turn,
# inspects whatever session file it left behind, then tries to resume
# that session_id and documents exactly what happens.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SANDBOX="$SCRIPT_DIR/sandbox"
OUT="$SCRIPT_DIR/out"
CLAUDE="$HOME/.local/bin/claude"

mkdir -p "$SANDBOX" "$OUT"

echo "=== 02-kill-midturn.sh ==="

# Compute the ~/.claude/projects/<slug> dir claude will use for $SANDBOX,
# based on the naming scheme observed: cwd with '/' -> '-'.
SLUG=$(echo "$SANDBOX" | sed 's#/#-#g')
PROJECT_DIR="$HOME/.claude/projects/$SLUG"
echo "   expecting project dir: $PROJECT_DIR"

STREAM_FILE="$OUT/02-stream-raw.jsonl"
: > "$STREAM_FILE"

echo "-> starting long streaming turn in background (--include-partial-messages so we can catch a real mid-text-generation moment)"
(cd "$SANDBOX" && "$CLAUDE" -p "Count from 1 to 100, one number per line, thinking briefly about each before saying it" --model haiku --output-format stream-json --verbose --include-partial-messages > "$STREAM_FILE" 2>"$OUT/02-stream-stderr.log") &
CLAUDE_BG_PID=$!
echo "   backgrounded pid=$CLAUDE_BG_PID"

echo "-> polling for a genuine text_delta stream_event (proof the model is actively streaming a text content block)"
FOUND_DELTA=0
for i in $(seq 1 60); do
  sleep 0.5
  if grep -q '"type":"text_delta"' "$STREAM_FILE" 2>/dev/null; then
    FOUND_DELTA=1
    echo "   observed a text_delta stream_event after ~$((i / 2))s, killing NOW"
    break
  fi
done

LINES=$(wc -l < "$STREAM_FILE" 2>/dev/null | tr -d ' ')
LINES=${LINES:-0}

if [[ "$FOUND_DELTA" -eq 0 ]]; then
  echo "FAIL: never observed a text_delta stream_event before timeout (lines captured: $LINES)"
  kill -9 "$CLAUDE_BG_PID" 2>/dev/null
  exit 1
fi

echo "-> SIGKILL pid=$CLAUDE_BG_PID mid-text-generation (lines so far: $LINES)"
kill -9 "$CLAUDE_BG_PID" 2>/dev/null
sleep 1
# also kill any child claude processes it may have spawned (defensive)
pkill -9 -f "claude .*Count from 1 to 100" 2>/dev/null

wait "$CLAUDE_BG_PID" 2>/dev/null
echo "   process reaped"

echo "-> lines captured in stream file: $(wc -l < "$STREAM_FILE" | tr -d ' ')"
echo "-> last stream-json line captured:"
tail -n 1 "$STREAM_FILE" || true

echo "-> looking for session file under $PROJECT_DIR"
if [[ ! -d "$PROJECT_DIR" ]]; then
  echo "FAIL: expected project dir does not exist: $PROJECT_DIR"
  exit 1
fi

SESSION_FILE=$(ls -t "$PROJECT_DIR"/*.jsonl 2>/dev/null | head -n 1)
if [[ -z "$SESSION_FILE" ]]; then
  echo "FAIL: no session .jsonl file found in $PROJECT_DIR"
  exit 1
fi
echo "   newest session file: $SESSION_FILE"
cp "$SESSION_FILE" "$OUT/02-session-file-post-kill.jsonl"

SESSION_ID=$(basename "$SESSION_FILE" .jsonl)
echo "   session_id (from filename): $SESSION_ID"
echo "   lines in on-disk session file: $(wc -l < "$SESSION_FILE" | tr -d ' ')"
echo "-> last line of on-disk session file (state at kill time):"
tail -n 1 "$SESSION_FILE" || true

echo ""
echo "-> attempting resume of killed session"
RAW_RESUME=$(cd "$SANDBOX" && "$CLAUDE" -p --resume "$SESSION_ID" "What was the last number you said?" --model haiku --output-format json 2>"$OUT/02-resume-stderr.log")
RESUME_EXIT=$?
echo "$RAW_RESUME" > "$OUT/02-resume-result.json"
echo "   resume exit code: $RESUME_EXIT"
echo "   resume stdout:"
echo "$RAW_RESUME"
echo "   resume stderr (if any):"
cat "$OUT/02-resume-stderr.log" 2>/dev/null || true

echo ""
echo "-> session file AFTER resume attempt:"
cp "$SESSION_FILE" "$OUT/02-session-file-post-resume.jsonl"
echo "   lines now: $(wc -l < "$SESSION_FILE" | tr -d ' ')"

if [[ $RESUME_EXIT -eq 0 ]]; then
  echo "RESULT: resume after SIGKILL mid-turn SUCCEEDED (exit 0). See NOTES.md for whether partial turn content was preserved/dropped."
else
  echo "RESULT: resume after SIGKILL mid-turn FAILED (exit $RESUME_EXIT). See NOTES.md for details."
fi

exit 0
