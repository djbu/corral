# corral resume spike — session-file forensics

Environment: `claude` 2.1.224 (`~/.local/bin/claude`), all invocations `--model haiku`,
cwd pinned to `spike/resume/sandbox/` so session files land under one isolated
`~/.claude/projects/` entry. Raw captures in `spike/resume/out/`, golden corpus in
`testdata/golden/streamjson/`.

## 1. `~/.claude/projects/` naming scheme

- One directory per distinct **cwd**, named by taking the absolute cwd path and
  replacing every `/` with `-`. Example observed:
  `/Users/danielbecerra/code/research/corral/spike/resume/sandbox`
  → `~/.claude/projects/-Users-danielbecerra-code-research-corral-spike-resume-sandbox/`
  No hashing, no truncation seen — it's a literal, reversible slug. (A repo with a
  literal `-` in a path segment would presumably collide/ambiguate with a `/`,
  but we did not test that.)
- Inside that directory: one file per session, named `<session_id>.jsonl` where
  `session_id` is the UUID also embedded in every line's `sessionId` /
  `session_id` field. Filename and in-file session id always matched exactly in
  our 13 captured session files — verified programmatically.
- Format: JSONL, one JSON object per line, append-only. `--resume` on an existing
  session_id appends more lines to the *same* file (verified: a 2-turn
  round-trip produced one 20-line file under the original session_id, not two
  files).
- **Line/record types observed** (`.type` field):
  - `queue-operation` — `{operation: "enqueue"|"dequeue", sessionId, content, timestamp}`. Bookkeeping for the prompt queue; appears before the `user` message record for the same turn.
  - `attachment` — a grab-bag of session/hook plumbing (`attachment.type` sub-discriminates: `hook_success`, `hook_additional_context`, `deferred_tools_delta`, `agent_listing_delta`, `skill_listing`, etc). Not conversation content; corral's parser should skip these unless it wants hook telemetry.
  - `user` — an actual turn record: `{parentUuid, isSidechain, promptId, type:"user", message:{role:"user", content: <string or content-block array>}, uuid, timestamp, permissionMode, sessionId, cwd, version, gitBranch, ...}`. Tool-result turns show up as `type:"user"` with `message.content` = an array of `{type:"tool_result", tool_use_id, content, is_error}` blocks (see denial example below) rather than a distinct type.
  - `assistant` — same envelope shape but `message.role:"assistant"`, `message.content` is an array of content blocks (`text`, `thinking`, `tool_use`). One `assistant` record is written **per completed content block** (not per full message) when the CLI is actively streaming — e.g. a turn with thinking + text produces two `assistant` lines, each with the full accumulated content of that block.
  - `last-prompt` and `mode` — small session-state markers seen at the end of a resumed session's on-disk file (2nd resume trial); did not dig into their full schema, likely UI/CLI state (last prompt text cache, active permission mode) rather than conversation content.
  - (On the **stdout stream**, not the on-disk file, you additionally see `system` records — subtypes `hook_started`, `hook_response`, `init`, `status`, `thinking_tokens` — and, with `--include-partial-messages`, `stream_event` wrapping raw Anthropic API SSE events (`message_start`, `content_block_start/delta/stop`, `message_delta`, `message_stop`), plus a trailing `result` record and occasional `rate_limit_event`. Only a subset of this (`user`/`assistant`/`attachment`/`queue-operation`) gets persisted to the on-disk session `.jsonl` — the on-disk file is not simply "the stdout stream captured to a file.")

## 2. Fields corral's parser must care about

- `session_id` (top-level on `result`/`system:init`, or `.sessionId` inside stored records) — stable identity key for the registry. **Does not change across `--resume`** (see §5).
- On the `result` line (stdout, end of every `-p` invocation): `is_error`, `result` (the final text), `total_cost_usd`, `usage` (input/output/cache tokens), `permission_denials` (array — empty when nothing was denied), `duration_ms` / `duration_api_ms`, `num_turns`, `stop_reason`, `terminal_reason`.
- `tool_use` blocks: inside an `assistant` record's `message.content[]`, `{type:"tool_use", id, name, input, caller}`. corral needs `id` to correlate with the matching `tool_result`.
- `tool_result` / permission denial: comes back as a `type:"user"` record whose `message.content[]` has `{type:"tool_result", tool_use_id, content, is_error}`. A **denial** looks exactly like an error tool_result (`is_error:true`, `content` = a human-readable "blocked" message) — there is no separate "denied" record type. The authoritative signal that a denial occurred is the **`permission_denials` array on the final `result` line**: `[{tool_name, tool_use_id, tool_input}]`. Parsers must join `permission_denials` back to the `tool_use` id to know which call was blocked.
- `--include-partial-messages` (stream-json only) unlocks raw token-level `stream_event`s (`content_block_delta` with `text_delta`/`thinking_delta`/`signature_delta`) — needed only if corral wants live token-by-token UI; not needed for post-hoc parsing of the persisted transcript, because deltas are never written to disk (see §3).

## 3. Kill-mid-turn behavior (task 2) — precise answer

**Setup:** background `claude -p "Count from 1 to 100..." --output-format stream-json --verbose --include-partial-messages`, poll stdout for the first real `text_delta` `stream_event` (proof the model has started actively emitting the counted text), then `kill -9` immediately.

**Trial 1** (threshold: first 3 stream-json lines, which turned out to be pre-generation `system` records only): killed before the model call had even been dispatched. On-disk session file at kill time had 8 lines, all pre-turn bookkeeping (`queue-operation` ×2, `attachment` ×4 for hooks/tool-listing, `user` for the prompt). Zero assistant content.

**Trial 2** (tightened threshold: wait for an actual `text_delta`): stdout capture proved the model *did* fully stream a real answer — thinking block, then a text block containing "1" through "17" (confirmed via `jq` extraction of the deltas), then `content_block_stop`, `message_delta`, `message_stop` all appeared in the captured stdout before/around the kill. **Despite the full API turn having completed and streamed to stdout, the on-disk session `.jsonl` file was still frozen at 8 lines — the exact same pre-turn bookkeeping set as trial 1 — with no `assistant` record for the counting turn at all.**

**Conclusion, stated precisely:**
- The on-disk session file is **not** updated incrementally as tokens/blocks stream. There is a real lag between "the CLI has received (and even shown on stdout) a fully-completed assistant message" and "that message is durably appended to the session `.jsonl`." A kill inside that lag window **loses the entire in-flight turn**, not just the not-yet-generated tail of it.
- The loss is **clean, not corrupt**: no partial/truncated JSON line, no dangling record — the file simply ends at the last fully-flushed pre-turn record. `jq` parses it with zero errors both times.
- **Resume still works mechanically** (`--resume <session_id>` exits 0, session_id is preserved, the file is appended to going forward), but semantically the model has **zero memory that the killed turn ever happened**. Asked "what was the last number you said?" it answered "None—didn't count" (trial 1) / "Haven't said any. Refused initial count request." (trial 2) — not a guess, not a hallucinated continuation, a correct read of what's actually in its context (nothing).
- **Implication for corral's crash-recovery design:** you cannot assume "if the API finished generating, the turn is safe." A daemon that supervises `claude` and wants at-least-once delivery of assistant output must treat *any* SIGKILL/crash between "user message sent" and "next prompt accepted" as a **total loss of that turn's assistant output**, even if the model had already produced (and even already streamed to the terminal) a complete, well-formed answer. If corral needs that output durably, it must capture the CLI's own stdout stream itself (own tee/log), not rely on re-reading the session file after a crash — the session file will not contain the lost turn, and there's no way to distinguish "turn never started" from "turn fully completed but not yet persisted" by inspecting the file alone. The safe design is: re-send the same prompt on resume if the disk file doesn't show a matching assistant record, and expect to pay for regenerating it.
- One nuance we could not cleanly isolate: whether the persistence lag is "flushed once per full turn" or "flushed once the whole tool-loop/`num_turns` sequence completes." Both trials only involved a single API turn, so we can't rule out that *even more* could be pending flush in a multi-tool-call turn. Treat the finding as a lower bound on loss, not an upper bound.

## 4. Version-specific quirks / docs vs. reality

- **`-p` default permission mode is NOT deny-by-default in this environment**, contradicting the task brief's assumption. `claude -p 'Run `echo x` using the Bash tool...'` with no `--allowedTools` and no `--permission-mode` flag **succeeded** (`is_error:false`, `permission_denials:[]`, tool actually ran). This traced back to this user's global `~/.claude/settings.json` having `"skipAutoPermissionPrompt": true` — apparently causing Bash calls whose targets stay inside the current project root to auto-approve even with an empty allowlist, in headless `-p` mode. Explicitly passing `--permission-mode manual` did **not** change this (still auto-approved). `--disallowedTools "Bash"` doesn't produce a denial either — it removes the tool from the model's toolset entirely, so the model just reports it has no Bash tool (no `permission_denials` entry, no `tool_use`/blocked `tool_result` pair at all).
  - **The one guardrail that reliably still fires regardless of allow/deny lists**: the built-in path-safety restriction that blocks `Bash`/file-tool operations targeting paths *outside the current project root*. `Run \`rm -rf /tmp/...\`` (a path outside the sandbox) produced a real, well-formed denial: a `tool_use` for `Bash`, a `user`/`tool_result` with `is_error:true` and a human-readable "blocked, may only operate in allowed working directories" message, and a populated `permission_denials: [{tool_name, tool_use_id, tool_input}]` on the final `result` line. This is what `testdata/golden/streamjson/error-turn.jsonl` captures.
  - **Corral implication:** don't assume a stock/CI environment will behave like a personal dev machine's `-p` invocations for permission testing — global user settings (`skipAutoPermissionPrompt`, allow/deny lists, hooks) materially change tool-approval behavior even in headless mode. Corral should either pin a known settings file for its supervised sessions or treat permission-denial behavior as environment-dependent and test against the actual target settings, not assumptions from the docs.
- `--include-partial-messages` is required to see token-level `stream_event`s at all; without it, `--output-format stream-json --verbose` only emits one `assistant` record per *completed content block*, not per token. The task brief's phrasing ("first few stream-json events arrive") undersells how much bookkeeping (`system` hook/init/status events) precedes any real model output — in trial 1 the first 3 events were 100% pre-generation.
- This user's session carries a large amount of non-Claude-Code-core noise in every captured transcript: a `caveman` plugin hook injecting a "CAVEMAN MODE ACTIVE" system prompt via `SessionStart`/`UserPromptSubmit` hooks, a `headroom` MCP server, and various `claude.ai` connector MCP servers all showing `needs-auth`/`pending`. None of this is specific to `claude`/`-p`/`--resume` behavior — it's this machine's global config — but it does mean the golden corpus's `system:init` and early `attachment` records are noisier/larger than a stock install would produce. Not sanitized per instructions, but worth knowing when writing parser tests: don't assume a small, fixed `tools` list in `system:init`.
- `--allowedTools` accepts either one comma/space-separated string or multiple `--allowedTools` args (variadic `<tools...>`); both syntaxes worked in `03-golden.sh`.
- One `-p` invocation printed `Warning: no stdin data received in 3s, proceeding without it. ...` to stderr even though stdin was the script's own terminal (not explicitly redirected) — harmless, but stderr is not clean stdout-only JSON; anything shelling out to `claude -p` should keep stdout and stderr separated and only parse stdout.

## 5. session_id across `--resume`: unchanged

Directly checked both from the JSON responses and from the files on disk:

- Task 1 (`01-roundtrip.sh`): turn 1 returned `session_id=d389bd22-4b28-4b3f-bb1c-40e3213fa261`; turn 2 (`--resume d389bd22-...`) returned the **same** `session_id` in its JSON result. On disk, both turns live in the single file `d389bd22-4b28-4b3f-bb1c-40e3213fa261.jsonl` (20 lines total, all records carrying `sessionId:"d389bd22-..."`).
- Golden corpus `resumed-session.jsonl`: base session created as `ae78edc8-0ef7-41cc-be75-329e99a11b69`; the `--resume` stream-json capture's `result` line reports the identical `session_id`.
- Across all 13 session files this spike produced under the sandbox project dir, filename (`<uuid>.jsonl`) and every in-file `sessionId`/`session_id` field matched exactly — no case of a resumed session silently migrating to a new id.

**Corral implication:** the registry can key supervised sessions by `session_id` for their entire lifetime; `--resume` is a pure continuation of the same identity, not a fork/rename. (We did not test `--resume` racing with a *second* concurrently-live process on the same session_id — that's a distinct open question for corral's locking design, out of scope for this spike.)

## Task results summary

- Task 1 (basic round-trip): **PASS**. See `spike/resume/out/01-turn1.json`, `spike/resume/out/01-turn2.json`.
- Task 2 (kill mid-turn): mechanically **PASS** (resume succeeds, exit 0, no corruption) but the interesting finding is semantic, see §3. Raw artifacts: `spike/resume/out/02-stream-raw.jsonl` (stdout captured before kill), `spike/resume/out/02-session-file-post-kill.jsonl`, `spike/resume/out/02-session-file-post-resume.jsonl`, `spike/resume/out/02-resume-result.json`.
- Task 3 (golden corpus): **PASS**, 5/5 files captured in `testdata/golden/streamjson/`:
  - `simple-text-reply.jsonl` — 10 lines
  - `tool-use-bash.jsonl` — 12 lines
  - `tool-use-write-read.jsonl` — 19 lines
  - `resumed-session.jsonl` — 9 lines
  - `error-turn.jsonl` — 18 lines (permission denial via the outside-project-root guardrail, not via omitted `--allowedTools`; see §4)
