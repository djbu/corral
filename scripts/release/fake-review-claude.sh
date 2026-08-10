#!/usr/bin/env bash
set -euo pipefail

: "${CORRAL_M8_FAKECLAUDE:?CORRAL_M8_FAKECLAUDE is required}"
printf 'change produced inside an isolated task worktree\n' > m8-review.txt
git add -- m8-review.txt
git -c user.name='corral M8 smoke' -c user.email='m8-smoke@invalid' \
  commit -q -m 'M8 review fixture'
exec "$CORRAL_M8_FAKECLAUDE" "$@"
