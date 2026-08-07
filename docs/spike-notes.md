# M0 spike notes — consolidated findings

Environment: macOS arm64, Go 1.26.4, claude CLI 2.1.224. Full detail lives in
`spike/pty/NOTES.md` and `spike/resume/NOTES.md`; this file is the executive
summary plus the design consequences already folded into RUNBOOK.md.

## Verdict

Both risky primitives work. M0 exit criteria met. No blockers for M1.

- PTY ownership: daemon detached via setsid re-exec owns a `claude` TUI in a
  PTY; attach/detach/reattach over a unix socket, resize propagation — all
  proven with automated demos (`spike/pty/demo-output.txt`,
  `spike/pty/claude-demo-report.txt`).
- Kill/resume: `--resume <session_id>` is a pure continuation of the same
  identity — session_id stable for the session's whole lifetime, files
  append-only, no corruption even after SIGKILL mid-turn.

## Findings that changed the design

1. **Session files are not flushed incrementally** (resume spike §3). A crash
   between "user message sent" and the post-turn flush loses the whole
   in-flight turn — cleanly — even when the API had already streamed the full
   answer. → M3: corral tees the CLI's stdout as the authoritative in-flight
   record; reaper checkpoints only at flush-verified turn boundaries; recovery
   re-sends the last prompt when the assistant record is missing on disk.
2. **Raw byte replay cannot restore alt-screen TUIs** (pty spike, finding 1).
   Ring-buffer replay reattached `vi`/`claude` into corrupted screens
   (mid-escape-sequence, mid-UTF-8-rune truncation). Bash-only testing hides
   this completely. → M1: the attach layer needs a headless terminal emulator
   tracking screen-grid state per session; raw scrollback is only acceptable
   for logs, never for reattach.
3. **Permission behavior in `-p` is environment-dependent** (resume spike §4).
   Global user settings (`skipAutoPermissionPrompt`, allowlists, hooks) leak
   into headless runs. → M1/M2: corral pins an explicit settings file per
   supervised session; the harness never assumes stock behavior.
4. **Child environment must be constructed, not inherited** (pty spike,
   finding 3). `os.Environ()` passthrough leaked session markers that visibly
   changed the child claude's behavior. → M1: minimal explicit env whitelist.
5. **Detach signaling is client-side only** (pty spike, finding 5). The daemon
   cannot distinguish clean detach from client crash, and a bare control byte
   can collide with app input. → M1: tmux-style prefix-key detach plus a
   socket-level goodbye frame; treat missing goodbye as a crash, not a detach.

## Facts the implementation can rely on

- `~/.claude/projects/<cwd with "/"→"-">/<session_id>.jsonl`, append-only,
  one file per session; filename always equals in-file session id (13/13).
- `result` line carries: `is_error`, `total_cost_usd`, `usage`,
  `permission_denials[]`, `num_turns`, `duration_ms`, `stop_reason`.
- Denials are ordinary `tool_result` errors; the authoritative signal is
  `permission_denials` on `result`, joined back via `tool_use_id`.
- On-disk records ≠ stdout stream: deltas and `system` events never persist;
  one `assistant` record per completed content block.
- claude TUI startup: alt-screen `?1049h`, 4 mouse modes, bracketed paste,
  focus reporting, DEC mode 2031 (in-band resize), OSC title — and no cursor
  or background queries, no trust prompt in a fresh directory.
- `pty.StartWithSize` + `Setsid` re-exec pattern works for daemonization on
  darwin; stderr of `claude -p` is not clean (warnings) — parse stdout only.

## Golden corpus

`testdata/golden/streamjson/` seeded with 5 real transcripts (simple text,
bash tool use, write/read tool use, resumed session, permission denial).
Captured on claude 2.1.224 with this machine's global config — `system:init`
and early `attachment` records carry local hook/MCP noise; parser tests must
not assume a fixed `tools` list.

## Open questions carried forward

- Persistence-lag granularity: is the flush per API turn or per full
  tool-loop? Both trials were single-turn; treat observed loss as a lower
  bound (M3 must handle worse).
- Two live processes racing on one session_id via `--resume`: untested;
  corral's registry needs its own locking regardless (M1 store design).
- Real raw-mode terminal edge cases on attach: socket-level proven; human
  terminal validation happens naturally once M1 gives us `corral attach`.
