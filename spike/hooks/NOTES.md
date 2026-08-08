# M2 Step 0 — Hook behavior verification (CLI 2.1.224)

Binary under test: `~/.local/share/claude/versions/2.1.224` (the `~/.local/bin/claude`
symlink currently points at 2.1.225, so all runs invoked the versioned path directly).

Budget: **9 of 12 billed prompts used** (Runs A, A2, B, C, D, D2, D3, D4, E — one
prompt each). Run F (`sigtermend`) cost 0 — SIGTERM before the model produces any
turn. 3 prompts remain unspent.

Method follows `spike/verify/NOTES.md`'s format. Harness: `spike/hooks/capture/main.go`
(`corral-hookcap`), a generic hook-capture binary wired via `--settings` (see
`spike/hooks/gen_settings.py`) that appends one JSON line per invocation — argv, pid/ppid,
`CORRAL_`/`CLAUDE_`/`ANTHROPIC_`-prefixed env, raw+parsed stdin, timestamp — to a run file
under `spike/hooks/out/`. Headless runs via `spike/hooks/run_headless.sh` (`-p`, explicit
env allowlist, never `os.Environ()`/`env` wholesale-inherit). TUI-only items via
`spike/hooks/pty/main.go` (`corral-hookspty`), modeled on `spike/verify/main.go`'s PTY
driver pattern, pid-scoped process-group kill only. Sandbox: `spike/hooks/sandbox/`
(+ `tui`..`tui6` variants), each a throwaway git-init'd repo. User's real `~/.claude`
was never written to — only read (to discover the real caveman hooks for the merge test)
and only ever touched via `--settings <file>` + sandbox cwd.

Cleanup: all self-spawned pids (94267, 97539, 1564, 3727, 7203, 10931, 10962, plus the PTY
children of runs D–F) confirmed dead via `kill -0` at close. Zero orphans. Three unrelated
pre-existing `claude` processes on the machine (24707, 52421, 19386) were left untouched.

## LOUDLY: design-relevant findings

1. **Hook event count is 15, not the design's provisional 9.** Beyond
   `SessionStart/UserPromptSubmit/PreToolUse/PostToolUse/Notification/Stop/SubagentStop/PreCompact/SessionEnd`,
   the binary and the user's real `~/.claude/settings.json` both reference
   `PermissionRequest`, `PostToolBatch`, `PostToolUseFailure`, `StopFailure`,
   `SubagentStart`, `TeammateIdle`. Of these extras, this spike actually observed
   `PermissionRequest` and `SubagentStart` firing live; `PostToolBatch`,
   `PostToolUseFailure`, `StopFailure`, `TeammateIdle`, and `PreCompact` never fired in
   any run (no batched tool calls, no failing tool calls, no failing Stop-hook-of-a-Stop,
   no idle teammate, no compaction attempted — all UNVERIFIED, not contradicted). The
   hooks state engine's event enum needs to plan for at least these 15, not 9.

2. **`PermissionRequest` carries the decision inputs directly** — `tool_name`,
   `tool_input`, and a `permission_suggestions` array (`addDirectories`/`setMode` with
   `destination`) — see `testdata/hooks/2.1.224/permissionrequest-bash.json`. If the
   design's dangling-tool-call tracker or auto-approval logic was going to reconstruct
   this from `PreToolUse` + `Notification` alone, it doesn't have to: this event exists,
   fires between them, and already has the structured shape.

3. **Notification firing does NOT mean a human is looking at a dialog.** This account has
   "Auto Mode" active (a distinct feature from `--permission-mode`, inspectable via
   `claude auto-mode config`), which silently auto-approves tool calls — including a
   destructive `rm -rf ./victim` in one probe — with **no dialog ever rendered** (confirmed
   by replaying the raw PTY bytes through a real VT100 emulator, `pyte`, not naive
   ANSI-stripping regex, which can produce false negatives). Despite this, `Notification`
   with `notification_type:"permission_prompt"` still fires (see
   `testdata/hooks/2.1.224/notification-permission.json`). Any design logic that treats
   "Notification fired" as "a human is now blocked waiting" is wrong on accounts with Auto
   Mode (or any other silent-approval path) enabled. It was NOT possible, across 5 attempts
   under varied commands/settings-sources, to force a real interactive dialog to appear —
   V16 (dialog shape) stays UNVERIFIED for that reason, not because Notification is unreliable.

4. **`--settings`-injected hooks merge with the user's real hooks at the array level; they
   do not replace them.** For the same event name (`SessionStart`, `UserPromptSubmit`
   confirmed), both hook groups fire. This closes the gap M1's `spike/verify/NOTES.md`
   (V2) explicitly left open. Confirmed alongside: `--setting-sources project,local`
   (excluding `user`) makes the real caveman hook vanish while `--settings`-injected hooks
   are unaffected — these are independent gating mechanisms, and a design that assumes
   "our `--settings` hooks are the only hooks running" is wrong whenever the user's own
   `~/.claude/settings.json` has hooks configured (this machine's does).

No evidence found for the two other loudly-flagged failure modes the task asked about:
env IS inherited by hooks (see V10 below), and `--settings` hooks were never observed to be
replaced (only merged) by user hooks.

## Verdict table

| # | Question | Verdict |
|---|---|---|
| V4 | Does `--settings`-injected `hooks{}` merge with or replace the user's real hooks for the same event? | **Merges** (array-level, both fire). Confirmed on SessionStart + UserPromptSubmit. |
| V5 | (load-bearing, folded into V4/V17) settings-layer precedence | `--settings` hooks are independent of `--setting-sources`; user/project/local hooks are gated by `--setting-sources`, `--settings` hooks are not. |
| V6 | Does `Notification` fire for a pending permission prompt in interactive TUI mode? | **Yes, it fires** — but fires even when Auto Mode silently auto-resolves the request without ever showing a dialog. Firing is not proof a human is blocked. |
| V7 | Is `tool_use_id` present on PreToolUse/PostToolUse for correlation? | **Yes**, present from the very first run (see `pretooluse-bash.json`: `tool_use_id:"toolu_01LjYJv1nneLAZXt6Chdc5LS"`). |
| V8 | Hook timeout: field name/units/on-timeout behavior/default when omitted | Field is `timeout` (seconds) inside each hook command object. Behavior-on-timeout not empirically triggered (never set one low enough to fire). Default numeric value when `timeout` is omitted **not conclusively found** — binary strings only surfaced an unrelated background-runner `--hook-timeout` flag from a different subsystem. UNVERIFIED for the exact default. |
| V9 | Hook stdout JSON-output schema | Fully recovered from binary strings, zero cost: `continue`, `suppressOutput`, `stopReason`, `decision` (`approve`/`block`), `reason`, `systemMessage`, `terminalSequence`, `permissionDecision` (`allow`/`deny`/`ask`, `defer` is print-mode-only), `hookSpecificOutput` (per-event sub-schema for PreToolUse/UserPromptSubmit/PostToolUse/PostToolBatch/Stop+SubagentStop). |
| V10 | Is the daemon's environment inherited by hook subprocesses? | **Yes.** Marker env vars (`CORRAL_HOOKCAP_TESTVAR`, `CORRAL_SESSION_ID_FAKE`) appeared in every hook invocation across every run, including subagent-context hooks. |
| V11 | Subagent (Task tool) hook attribution — whose `session_id`? | Fires under the **parent's** `session_id`; carries `agent_id`/`agent_type` instead (see `subagentstart.json`: `agent_type:"general-purpose"`). Env still inherited. One SubagentStop sample observed with `agent_type:""` (empty) — see `subagentstop-empty-agenttype.json`; cause not chased further (budget), noted as a shape variant. |
| V12 | `SessionStart` `source` values for fresh vs. resumed session | `source:"startup"` on a fresh session, `source:"resume"` on `--resume`. Both captured as fixtures. |
| V13 | Matcher syntax for tool events | `"*"` works empirically (undocumented). Binary strings show the officially documented all-match syntax is empty string `""`, not `"*"`. Both are functionally match-all; only `"*"` was exercised live. |
| V14 | `stop_hook_active` — does a blocking Stop hook cause Stop to refire with `stop_hook_active:true`? | **Yes**, confirmed for the block→continue→re-Stop path via the exit-code-2 probe, at zero extra billed prompts (see `stop-hook-active-true.json`). The **denial** sub-case (does a denied PermissionRequest also end the turn this way) is **UNVERIFIED** — Run E's attempt to reach a real dialog to deny was inconclusive (Auto Mode pre-empted it; see finding #3) and was not retried to conserve budget. |
| V15 | Is `Notification` immediate or delayed relative to `PreToolUse`/`PermissionRequest`? | **Delayed**, ~6s observed gap in Runs D2/D4. The delay is Auto Mode's own evaluation window, not human latency — do not conflate the two when the design models expected human response time. |
| V16 | Interactive permission dialog shape / approve keystroke | **UNVERIFIED.** No real dialog frame was ever observed (confirmed via `pyte` VT100 replay, not just naive detection failure) across 5 attempts with varied commands, `--setting-sources`, and `--permission-mode`. Root cause: this account's Auto Mode configuration reliably auto-resolves before a dialog renders. |
| V17 | Does `--setting-sources` excluding `user` also exclude `--settings`-injected hooks? | **No.** Excluding `user` (`--setting-sources project,local`) makes the real caveman hook vanish; `--settings`-injected hooks are unaffected. Independent mechanisms — confirmed. |
| V18 | Does `SessionEnd` fire on SIGTERM in TUI mode? | **Yes**, with `reason:"other"` (see `sessionend-other.json`). Free — no billed prompt needed (Run F). |
| V19 | (free item) | Covered opportunistically alongside other runs; no separate probe needed beyond what's captured above. |
| V20 | Matcher-less form for non-tool events | Confirmed functional — non-tool event hook entries with no `matcher` key at all fire normally. |
| V21 | Is the interactive permission dialog deterministic (same command → same dialog, every time)? | **False / not deterministic**, contingent on Auto Mode configuration. On this account it does not reliably appear at all. A design that assumes "if the tool needs permission, a dialog will render" needs a fallback for accounts where it silently doesn't. |
| PreCompact | (not in original priority list, mentioned as skippable) | **UNVERIFIED / not attempted** — not cheaply triggerable within remaining budget; no evidence for or against the design's assumptions about it. |

## Fixtures captured

15 distinct payload files written to `testdata/hooks/2.1.224/`, split out of the raw
`spike/hooks/out/run{A,A2,B,C,D,D2,D3,D4,F}.jsonl` capture files via
`spike/hooks/split_fixtures.py`:

- `sessionstart-startup.json`, `sessionstart-resume.json`
- `userpromptsubmit.json`
- `pretooluse-bash.json`, `pretooluse-agent-task.json`
- `posttooluse-bash.json`, `posttooluse-agent-task.json`
- `permissionrequest-bash.json`
- `notification-permission.json`
- `stop-normal.json`, `stop-hook-active-true.json`
- `subagentstart.json`, `subagentstop.json`, `subagentstop-empty-agenttype.json`
- `sessionend-other.json`

Distinct `hook_event_name` values actually observed firing across all runs (10 of the
15 known event names — see finding #1 for the other 5, which never fired):
`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PermissionRequest`,
`Notification`, `Stop`, `SubagentStart`, `SubagentStop`, `SessionEnd`.

## Budget

9 of 12 billed prompts consumed (Runs A, A2, B, C, D, D2, D3, D4, E). Run F free
(SIGTERM before any model turn). 3 prompts unspent — intentionally not spent chasing a
6th real-dialog attempt for V16 given finding #3's root cause is now understood (Auto
Mode), not a flaky detection method.
