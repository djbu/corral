#!/usr/bin/env bash
# 03-golden.sh — capture golden stream-json transcripts for the future
# corral parser's test suite. Byte-real, unsanitized. Prompts are kept
# free of anything sensitive.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SANDBOX="$SCRIPT_DIR/sandbox"
GOLDEN_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)/testdata/golden/streamjson"
CLAUDE="$HOME/.local/bin/claude"

mkdir -p "$SANDBOX" "$GOLDEN_DIR"

echo "=== 03-golden.sh ==="
echo "   golden dir: $GOLDEN_DIR"

echo "-> simple-text-reply.jsonl"
(cd "$SANDBOX" && "$CLAUDE" -p "Say hello in exactly three words." --model haiku --output-format stream-json --verbose) > "$GOLDEN_DIR/simple-text-reply.jsonl"
echo "   lines: $(wc -l < "$GOLDEN_DIR/simple-text-reply.jsonl" | tr -d ' ')"

echo "-> tool-use-bash.jsonl"
(cd "$SANDBOX" && "$CLAUDE" -p 'Run `echo golden-test` using the Bash tool and tell me its output' --model haiku --output-format stream-json --verbose --allowedTools "Bash(echo:*)") > "$GOLDEN_DIR/tool-use-bash.jsonl"
echo "   lines: $(wc -l < "$GOLDEN_DIR/tool-use-bash.jsonl" | tr -d ' ')"

echo "-> tool-use-write-read.jsonl"
(cd "$SANDBOX" && "$CLAUDE" -p 'Write the text "golden write-read spike" to a file named golden.txt in the current directory, then read the file back and tell me its contents.' --model haiku --output-format stream-json --verbose --allowedTools "Write" "Read") > "$GOLDEN_DIR/tool-use-write-read.jsonl"
echo "   lines: $(wc -l < "$GOLDEN_DIR/tool-use-write-read.jsonl" | tr -d ' ')"

echo "-> resumed-session.jsonl (first create a base session, then resume it under stream-json)"
BASE_RAW=$(cd "$SANDBOX" && "$CLAUDE" -p "Remember the word banana. Reply only: OK" --model haiku --output-format json)
BASE_SESSION_ID=$(echo "$BASE_RAW" | jq -r '.session_id')
echo "   base session_id: $BASE_SESSION_ID"
(cd "$SANDBOX" && "$CLAUDE" -p --resume "$BASE_SESSION_ID" "What word did I tell you to remember? Reply with only the word." --model haiku --output-format stream-json --verbose) > "$GOLDEN_DIR/resumed-session.jsonl"
echo "   lines: $(wc -l < "$GOLDEN_DIR/resumed-session.jsonl" | tr -d ' ')"

echo "-> error-turn.jsonl (permission denial)"
# NOTE: in this environment, plain 'no --allowedTools' does NOT deny Bash by
# default in -p mode (see NOTES.md: this user's global settings.json has
# skipAutoPermissionPrompt:true, which auto-approves in-project-root Bash
# calls even without an explicit allowlist). The one guardrail that reliably
# still fires is the built-in path-safety restriction: Bash commands that
# touch paths outside the current project root get a real permission_denials
# entry. Use that to capture a genuine denial turn.
(cd "$SANDBOX" && "$CLAUDE" -p 'Run `rm -rf /tmp/golden-denied-test-path` using the Bash tool (the path is outside the current project) and tell me what happened.' --model haiku --output-format stream-json --verbose) > "$GOLDEN_DIR/error-turn.jsonl"
echo "   lines: $(wc -l < "$GOLDEN_DIR/error-turn.jsonl" | tr -d ' ')"

echo ""
echo "=== golden corpus summary ==="
for f in "$GOLDEN_DIR"/*.jsonl; do
  echo "$(basename "$f"): $(wc -l < "$f" | tr -d ' ') lines"
done

exit 0
