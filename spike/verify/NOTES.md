# corral M1 Step-0 verifications (V1/V2/V3) — real `claude` CLI

Environment: `claude` 2.1.224 (`~/.local/bin/claude`), all real model invocations
`--model haiku`. Driver: `spike/verify/main.go` (Go, `creack/pty`, subcommands `v1`
and `v3`) plus inline bash for `v2`. Raw captures in `spike/verify/out/`
(gitignored, `spike/*/out/` pattern already covers it). Sandboxes under
`spike/verify/sandbox/{v1,v2,v3}/`.

Total real (billed) prompts used: **6** (budget was ≤6). Breakdown: V1×1,
V2×4 (two of the four were blown on permission-mode friction before landing
on a working invocation — see §2), V3×1.

**Methodological finding worth flagging on its own**: the first V1 run
silently produced *no* session `.jsonl` at all. Root cause: this driver runs
itself inside a live Claude Code session (me), and my own `os.Environ()`
carries `CLAUDE_CODE_CHILD_SESSION=1`. Passing that through to the spawned
`claude` (my first cut used `cmd.Env = os.Environ()`) makes the child print
*"Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker"* and
skip persistence entirely. This is a live demonstration of exactly the
finding m1.md §7.2 cites as "finding #3" — it's why the design mandates an
explicit env allowlist and never `os.Environ()`-derived construction. Fixed
in `main.go`'s `childEnv()` (allowlist only: `HOME,USER,LOGNAME,SHELL,PATH,
TMPDIR,LANG,LC_ALL,LC_CTYPE,TZ,SSH_AUTH_SOCK,ANTHROPIC_*` + corral-set
`TERM`/`COLORTERM`) and, for the bash-driven V2/V3 calls, `env -u
CLAUDE_CODE_CHILD_SESSION -u CLAUDE_CODE_SESSION_ID -u CLAUDE_CODE_ENTRYPOINT
-u CLAUDE_CODE_EXECPATH`. All results below are from the corrected runs.

---

## V1 — interactive session-id + `--resume` semantics

**VERDICT: direct-load** (not a picker). Design assumption in §3.5/§7.3 holds.

**Method**: `spike/verify/main.go v1`. Fresh UUID `44969786-60a1-...`. Phase 1:
launched interactive (no `-p`) `claude --session-id <uuid> --model haiku` in a
PTY, cwd `spike/verify/sandbox/v1`, sent `reply only OK\r`, drained until
quiet, then sent `/exit\r`. Phase 2: launched interactive `claude --resume
<uuid> --model haiku` in the same cwd/PTY, drained without sending any input,
then exited the same way.

**Evidence**:
- `/exit\r` cleanly terminated the interactive process both times — process
  exit observed within the 5s wait, no escalation to Ctrl-C/SIGTERM/SIGKILL
  needed. (`out/v1-run.log`: `exit method that worked: /exit, alive-after=false`
  for both phases.)
- Session file `~/.claude/projects/-Users-...-sandbox-v1/44969786-....jsonl`
  was created after phase 1 (24035 bytes, 16 records) — `out/v1-session-post-phase1.jsonl`.
- Phase 2's raw PTY capture (`out/v1-phase2-resume.raw`, 3707 bytes,
  de-escaped in this investigation) renders the **prior conversation
  directly**: `❯ reply only OK` / `⏺ OK` / `✻ Worked for 4s`, with a live,
  empty prompt box ready at the bottom — no session-list/picker UI, no
  "select a conversation" text (`grep -ai "select\|picker\|choose"` on the
  raw capture: zero matches).
- Session file after phase 2 grew from 16 → 17 lines, still under the
  **same** `sessionId` (`44969786-60a1-...` in both `out/v1-session-post-phase1.jsonl`
  and `out/v1-session-post-phase2.jsonl`) — pure continuation, no
  fork/rename, consistent with `spike/resume/NOTES.md` §5.

**Design impact**: none. §3.5's recovery path (`--resume <claude_session_id>`
on the argv, expecting direct load with no interaction required) is sound as
written.

**Extra check (free, 0 billed prompts)**: resumed the V3 session below
(`4e8e8f7a-...`, which ended on a **SIGTERM interrupt**, not a clean `/exit`)
via the new `corral-verify resumepeek` subcommand — same zero-input-drain
trick as phase 2 above, so no additional turn was billed. This is closer to
corral's actual §3.5 scenario than phase 2's cleanly-`/exit`-ed session was.
Result: still **direct-load, no picker** (`out/resumepeek-clean.txt`), and
the UI visibly surfaces the interrupted state on load — `⎿  Interrupted ·
What should Claude do instead?` right under the truncated
`"...4—Square corners. Stable platform. Four compass"` text, i.e. Claude Code
itself renders `isAbortedMidStream`/`interruptedByShutdown` as a first-class
resumable state, not just a persisted-but-invisible marker. Strengthens the
V1 verdict: direct-load holds for the restart-after-graceful-kill case, not
just the tidy-shutdown case.

---

## V2 — `--settings` precedence vs `~/.claude/settings.json` / project settings

**VERDICT: merge, key-by-key — not wholesale replacement.** For the
`CORRAL_PROBE` key the pinned `--settings` file names, its value wins over
project settings' value for that same key. But keys the pinned file does
*not* name are **not** dropped: the real user's hook config
(`~/.claude/settings.json`, live in this environment) kept firing identically
whether or not `--settings` was passed. This reverses my first-pass reading
of the same data (see below) — "wins outright / no merge" was ambiguous
between "wins per-key" and "replaces the whole settings object," and only
per-key-win is what the evidence actually shows.

**Method**: probe key `CORRAL_PROBE` (absent from real `~/.claude/settings.json`,
confirmed by inspection — no `env` key present there at all). Two competing
sources, deliberately different values, neither touching real user settings:
- `spike/verify/probe-settings.json` (the `--settings` file): `{"env":{"CORRAL_PROBE":"pinned-wins"}}`
- `spike/verify/sandbox/v2/.claude/settings.json` (project settings for that cwd): `{"env":{"CORRAL_PROBE":"project-wins"}}`

Ran `claude -p "printenv CORRAL_PROBE via Bash, report raw stdout" --model haiku
--output-format json` from inside `sandbox/v2`.

**Evidence, probe key** (`out/v2-*.json`):
| Run | `--settings` | `--setting-sources` | permission-mode | result |
|---|---|---|---|---|
| control | *(none)* | *(omitted → default)* | `bypassPermissions` | `project-wins` |
| test A | `probe-settings.json` (pinned-wins) | explicit `user,project,local` | `bypassPermissions` | `pinned-wins` |

Control confirms project settings load by default when no `--settings` flag
is given (`CORRAL_PROBE=project-wins`, matching the project file). Test A
shows that once `--settings` is added — even with `--setting-sources` set to
the *most inclusive* explicit value, `user,project,local`, which still
includes `project` — the pinned file's value wins for that key
(`pinned-wins`), not the project value.

**Evidence, discriminating merge from replace (free — data already on disk,
0 additional prompts)**: the pinned `probe-settings.json` contains *only*
`env` — no `hooks` key at all. The real `~/.claude/settings.json` on this
machine has live `UserPromptSubmit`/etc. hooks (the "CAVEMAN mode" plugin
hook seen rendering `[CAVEMAN]` in V1's raw capture). If `--settings`
**replaced** the settings object wholesale rather than merging per-key,
those user-level hooks should vanish the moment `--settings` is passed. They
don't:
```
$ grep -c CAVEMAN <control session, no --settings>.jsonl   → 2
$ grep -c CAVEMAN <test-A session, --settings pinned>.jsonl → 2
```
Both the control run (`efd5f7fb-...jsonl`) and test-A run (`ae432320-...jsonl`,
the one that *did* pass `--settings probe-settings.json`) contain an
identical `hook_additional_context` record — `hookEvent:"UserPromptSubmit"`,
content `"CAVEMAN MODE ACTIVE (full). ..."` — sourced only from the real
`~/.claude/settings.json`, which is never named on either command line and
not present in either the pinned or the project settings file. Confirms:
**`--settings` merges into the effective settings, overriding by key, not
replacing the object outright.** This directly validates §7.4's design,
which pins a settings file expecting *unlisted* keys (crucially, user-level
`apiKeyHelper`) to keep flowing from the user/project layers underneath it —
that assumption held.

**Open question, not tested**: whether an explicit empty override like
`{"hooks":{}}` (which is what §7.4 actually recommends pinning, "valid,
inert") *clears* the user's hooks map (top-level key replacement) or merges
at the array/handler level too. My probe file omitted `hooks` entirely, so
this only shows what happens to keys the pinned file doesn't mention, not
what happens when it explicitly names `hooks` with an empty value. §7.4's
"valid, inert" claim for `{"hooks":{}}` remains an assertion, not something
this session verified — worth a follow-up probe before relying on it.

**Scope note**: this test is pinned-vs-**project** (not pinned-vs-**user**
directly — the project sandbox at `sandbox/v2/.claude/settings.json` was the
competing file). The claim that `--settings` also beats plain user settings
for a given key is an **inference** by transitivity (project already outranks
user in Claude Code's own source ordering; pinned beat project ⇒ pinned beats
user), not a directly observed result. Flagging so the verdict isn't
overstated as "tested against user layer" when it wasn't, directly.

**Caveat — permission-mode friction burned 2 of the 4 V2 prompts**: this
environment's `~/.claude/settings.json` has `skipAutoPermissionPrompt: true`
(per `spike/resume/NOTES.md` §4), but that did **not** reliably auto-approve
Bash in `-p` mode here: `echo $CORRAL_PROBE` was hard-blocked by a Bash-tool
"variable expansion" safety guard (unrelated to permissions, unconditional),
and `printenv CORRAL_PROBE` without an explicit `--permission-mode` came back
as "approval needed" rather than auto-running. Both final tests above used
`--permission-mode bypassPermissions` to get a clean read — this is a
reasonable thing for a throwaway sandbox probe but means **the settings
precedence answer is not confounded by permission behavior**, since bypass
was explicit and identical across both control and test A.

**Caveat — omitted vs explicit `--setting-sources` not independently isolated**:
budget was exhausted (6/6 real prompts used) before a dedicated omit-vs-explicit
run with `--settings` held constant. Indirect evidence: control (omitted
`--setting-sources`, no `--settings`) and test A (explicit `user,project,local`,
with `--settings`) show no sign that specifying the flag changes which
source wins over `--settings` — but this is inference from two runs that
differ in two variables at once, not a controlled isolation. Flagging as
weaker evidence, not a verified independent finding.

**Design impact**: none required for §7.4's default
`setting_sources = "user,project,local"` — the worry that the pinned
`--settings` file might lose to project/user settings and need the default
narrowed (per §11's step-0 contingency: *"If it does not win, change the
`setting_sources` default to `project,local`"*) is **not realized**;
`--settings` wins for the keys it names, in this test. Better than that: the
*merge* behavior (not replace) is itself a load-bearing confirmation for
§7.4, which was relying on unlisted keys — chiefly user-level `apiKeyHelper`
— surviving underneath the pin. That reliance was previously untested and is
now supported. Two follow-ups worth doing before fully trusting §7.4,
though, neither of which is design-breaking: (1) verify `{"hooks":{}}`
specifically (the exact pin §7.4 recommends) is inert rather than clearing
the user's hook map — untested here, since my probe omitted `hooks`
entirely; (2) the pinned-vs-user comparison is inferred by transitivity, not
directly observed (see Scope note above) — cheap to close with one more
`-p` run pinning against a `~/.claude/settings.json`-only key with no
project file in the way. One thing worth a one-line addition to §7.4 or the
M1 README, though: **`--dangerously-skip-permissions`
/ `--permission-mode bypassPermissions` may be needed in practice for
headless-mode tool calls to auto-run**, since `skipAutoPermissionPrompt`
alone did not reliably do it in this environment — M2's actual policy
decisions on this are out of scope for M1's inert `{"hooks":{}}` pinned file,
but if M1 ever exercises Bash-tool behavior in tests/fakeclaude, don't assume
`skipAutoPermissionPrompt: true` is sufficient.

---

## V3 — SIGTERM flush semantics (M0 only tested SIGKILL)

**VERDICT: flushed, not lost, in `-p`/`sdk-cli` entrypoint mode** — SIGTERM
triggers an explicit, self-identifying partial-turn flush, categorically
different from SIGKILL's clean-but-total loss (`spike/resume/NOTES.md` §3).
**Exit latency: ~878 ms** after SIGTERM (no SIGKILL escalation needed).
**Caveat baked into the verdict, not just a footnote**: this was tested in
headless `-p` mode (`out/v3-session-post-sigterm.jsonl` records
`"entrypoint":"sdk-cli"`), because that's what `spike/resume/02-kill-midturn.sh`'s
method uses and what's cheap to script. Corral's M1 design (§7.3: "No `-p`,
no `--output-format`") spawns the **interactive TUI** entrypoint exclusively.
Whether the TUI installs the same SIGTERM flush handler is not directly
verified — it's a reasonable expectation (same binary, same signal), not a
tested fact. Do not read this verdict as "corral's actual restart path is
proven non-lossy"; read it as "the SIGTERM flush mechanism exists and works
in at least one entrypoint mode."

**Method**: `spike/verify/main.go v3`, replicating `spike/resume/02-kill-midturn.sh`'s
method (background `claude -p "Count from 1 to 30..." --model haiku
--output-format stream-json --verbose --include-partial-messages`, poll for
the first genuine `text_delta` `stream_event`) but signaling the **process
group** (`syscall.Kill(-pid, SIGTERM)`, matching corral's own
`procinfo.KillGroup`, since the child was spawned `Setsid: true` so
pgid == pid) instead of SIGKILL, with a 10s grace window before any SIGKILL
escalation.

**Evidence**:
- `out/v3-run.log`: text_delta observed ~8.5s in (19 stream lines captured);
  SIGTERM sent; process exited **877.5ms later** with `wait err=exit status
  143` (128+15, i.e. it died directly of SIGTERM, not a fallback KILL — no
  escalation was needed, well inside the 10s allowance). Worth stating
  affirmatively: §3.6/§8.3's `shutdown_grace = "5s"` default has roughly
  **5.7x headroom** over this observed flush-and-exit time — a
  design-validating number, not just a caveat-worthy one.
- Session file `out/v3-session-post-sigterm.jsonl` (11 records) **does**
  contain an `assistant` record for the counting turn (`has assistant
  record=true`), unlike SIGKILL's frozen-at-pre-turn-bookkeeping behavior.
- Byte-for-byte comparison: the streamed `text_delta` content up to the
  signal (`out/v3-stream-raw.jsonl`, reassembled: 191 chars, cut off mid-word
  — `"...4 — Square corners. Stable platform. Four compass"`) is **identical**
  to the `text` field persisted in the session file's `assistant` record —
  same 191 characters, same cutoff point. Nothing extra, nothing missing.
- Crucially, that persisted record carries explicit provenance the SIGKILL
  case never produces: `"isAbortedMidStream": true` on the `assistant`
  record, plus a synthetic follow-up `user` record,
  `{"content":[{"type":"text","text":"[Request interrupted by user]"}],
  "interruptedByShutdown": true}`. Claude Code itself recognizes and marks
  the SIGTERM-induced interruption as a first-class, disk-durable event —
  this is a real signal handler flushing state and appending an explicit
  marker, not an artifact of normal per-turn persistence timing.

**Design impact**: **this materially improves on the M1 pessimistic default**.
§12's known-limitation line ("A daemon restart while a session is mid-turn
loses that turn... Severity depends on verification V3") should be revised:
with SIGTERM (which is what corral's shutdown sequence already prefers,
§3.6/§3.7), the in-flight partial content up to the signal *is* durably
persisted with an explicit `isAbortedMidStream`/`interruptedByShutdown`
marker corral can detect — it is not a silent, undetectable loss like
SIGKILL. What corral still loses is only the *un-generated remainder* of the
turn (inherent to interrupting any in-progress generation, not a durability
bug), and the model has no memory that the turn was interrupted-and-resumed
unless corral re-prompts. Recommend updating §3.7 and §12 to state: SIGTERM
graceful shutdown is **not** lossless in the sense of "the full eventual
answer survives," but **is** lossless in the sense of "nothing silently
vanishes without a marker" — a meaningfully stronger guarantee than the
SIGKILL baseline the doc currently generalizes from. §11's step-0 framing
("Determines whether graceful restart is lossless or whether M1 documents
turn loss on every daemon upgrade") should note the answer is closer to
"partial-but-marked" than "lossy" — corral's recovery logic (§3.5) could use
`isAbortedMidStream`/`interruptedByShutdown` as a real signal to decide
whether to re-send the prompt on resume, rather than only relying on "no
matching assistant record" as `spike/resume/NOTES.md` §3 assumed would be
the only distinguishing case.

**Caveat**: single trial, single API turn, no tool calls in flight. Per
`spike/resume/NOTES.md` §3's own caveat, we did not test whether a
multi-tool-call turn behaves the same (more could be pending flush there).
Treat this as evidence SIGTERM's flush-on-signal handler exists and works
for the simple case, not a full characterization of all turn shapes.

**Caveat, entrypoint mode (see VERDICT line above)**: the SIGTERM signal
handling itself was only exercised via `-p`/`sdk-cli`, not the interactive
TUI corral actually spawns. Partially, not fully, closed by V1's free
resume-peek check (above): resuming this exact SIGTERM-killed session
through the **interactive TUI** (`--resume`, no `-p`) does render the
`isAbortedMidStream`/`interruptedByShutdown` marker correctly
(`⎿ Interrupted · What should Claude do instead?`) — so the *read/render*
side of this marker works in TUI mode. What's still unverified is the
*write* side: whether a TUI-mode `claude` process, killed mid-generation
with SIGTERM, flushes and marks the record the same way the `-p` process
did. Those are different code paths in principle (input loop vs. `-p`'s
single-shot loop) even though they likely share the same signal handler.

---

## Orphan / cleanup check

All PTY and background pids launched by this spike (V1: 92591, 92667; V3:
96885; resumepeek: 3168; V2's `-p` calls were synchronous foreground bash
subprocesses, already reaped by the time each command returned) were
confirmed dead (`kill -0 <pid>` failed for all) after each run, and `ps aux |
grep spike/verify` showed nothing lingering. Cleanup was pid-scoped only
throughout (`syscall.Kill(-pid, sig)` on pids/pgids this driver itself
started) — no name-based `pkill`, per the standing warning that this machine
runs unrelated `claude` processes.

## Files

- Driver: `spike/verify/main.go` (`go build -o corral-verify .`), `spike/verify/go.mod`
  (subcommands: `v1`, `v3`, `resumepeek`)
- V1 raw: `spike/verify/out/v1-phase{1,2}-{startup,after-prompt,full,resume}.raw`,
  `v1-session-post-phase{1,2}.jsonl`, `v1-run.log`
- V1 extra (resumepeek, free/0-billed): `spike/verify/out/resumepeek.raw`,
  `resumepeek-clean.txt`, `resumepeek-run.log`
- V2 inputs: `spike/verify/probe-settings.json`,
  `spike/verify/sandbox/v2/.claude/settings.json`; results:
  `spike/verify/out/v2-control.json`, `spike/verify/out/v2-testA.json`;
  merge-vs-replace evidence pulled directly from
  `~/.claude/projects/-Users-...-sandbox-v2/{efd5f7fb-...,ae432320-...}.jsonl`
  (not copied into `out/` — those are live session files, referenced by path
  and session id only)
- V3 raw: `spike/verify/out/v3-stream-raw.jsonl`, `v3-session-post-sigterm.jsonl`,
  `v3-run.log`, `v3-stderr.log`

## Summary table

| # | Verdict | Exit/latency evidence | Design impact |
|---|---|---|---|
| V1 | direct-load (no picker), incl. resuming a SIGTERM-interrupted session | `/exit` clean both phases, no escalation | none — §3.5/§7.3 sound as-is |
| V2 | merge, key-by-key: pinned wins for keys it names, unlisted keys (e.g. user hooks) survive | n/a | none required for §7.4 default; validates the unlisted-key-survives assumption; two untested follow-ups noted (`{"hooks":{}}` inertness, pinned-vs-user directly) |
| V3 | flushed-and-marked, not silently lost — tested in `-p`/sdk-cli mode only, TUI write-path unverified | SIGTERM→exit in 877.5ms (5.7x under the 5s `shutdown_grace` default), no KILL escalation needed | revise §3.7/§12 wording: partial-but-marked, not lossy, for the tested mode; recovery (§3.5) could consume `isAbortedMidStream`/`interruptedByShutdown`; recommend one more TUI-mode SIGTERM trial before fully generalizing |
