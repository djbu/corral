# Corral — Runbook

> Working name: **corral** (a pen where you keep your Claude agents). Rename freely.

A Claude-Code-only agent runtime: a daemon that supervises Claude Code sessions, knows their exact state via hooks, checkpoints them to disk, resumes them on demand, and lets you orchestrate and answer them from anywhere. Depth-over-breadth competitor to Herdr.

---

## 1. Positioning

| | Herdr | Corral |
|---|---|---|
| Agents supported | 19 CLIs, generic | Claude Code only |
| State detection | Terminal text scraping (heuristic) | Hooks + stream-json (exact, structured) |
| Persistence | Live PTY processes on a server | Checkpoint/resume via session files (`--resume`) — zero cost while idle, survives reboots |
| Blocked-agent flow | Remote keyboard into a terminal | Structured event → push notification → one-tap answer |
| Orchestration | Socket API around terminals | Native task DAG over `claude -p --input-format stream-json` |

**Thesis:** by giving up generality, every hard problem Herdr solves heuristically becomes a solved problem with a first-class API.

## 2. Architecture (daemon + client, dockerd-style)

One static Go binary, two roles:

```
corral daemon        # long-running supervisor (launchd/systemd unit)
corral <subcommand>  # thin client, talks to daemon over unix socket
```

```
┌─────────────────────────── corral daemon ───────────────────────────┐
│                                                                      │
│  Session Supervisor      State Engine           Task Orchestrator    │
│  - spawn/kill/attach     - hook event ingest    - task DAG           │
│  - PTY mgmt (attach UX)  - state machine per    - deps, retries      │
│  - headless (-p) runs      session:             - agent-spawns-agent │
│  - idle reaper             Working|Blocked(r)|                       │
│                            Idle|Dead            Notifier             │
│  Checkpoint Manager                             - ntfy/webhook/      │
│  - session-id registry   Store (SQLite)           Pushover           │
│  - resume on demand      - sessions, tasks,     - reply-to-permission│
│  - crash recovery          events, checkpoints    from phone         │
│                                                                      │
│  API server: HTTP+JSON over unix socket (opt: TCP + token + TLS)     │
└──────────────────────────────────────────────────────────────────────┘
         ▲                    ▲                          ▲
   corral CLI          hook helper (fired by       web dashboard /
   (attach, ls,        Claude Code hooks, POSTs    phone (later)
   run, answer)        events to the socket)
```

### Key design decisions

1. **Language: Go.** Workload is I/O-bound process supervision; iteration speed beats raw performance here. See §8 for the full rationale.
2. **Hooks are the source of truth for state.** On session creation, corral injects hook config into the project's `.claude/settings.local.json` (or `--settings` flag): `PreToolUse`, `PostToolUse`, `Notification`, `Stop`, `SessionStart`, `SessionEnd` hooks call a tiny `corral hook-relay` command that POSTs the JSON payload to the daemon socket. No terminal scraping, ever.
3. **Two execution modes per session:**
   - **Interactive (PTY):** full Claude Code TUI in a daemon-owned PTY; `corral attach` gives a tmux-like attach/detach. For humans driving sessions.
   - **Headless (task):** `claude -p --output-format stream-json --input-format stream-json` piped to the orchestrator. For automated tasks. Structured turn-by-turn events, no PTY needed.
4. **Checkpoint/resume is the persistence model.** Claude Code persists conversations under `~/.claude/projects/`. Corral records `(session_id, cwd, worktree, mode, last_state)`. The idle reaper kills processes idle > N minutes; `corral wake <name>` (or an incoming task/answer) relaunches with `claude --resume <session_id>`. Daemon restart = re-read SQLite, resume what was running. Nothing depends on a process staying alive.
5. **Store: SQLite** via `modernc.org/sqlite` (pure Go, no cgo, keeps cross-compile trivial). WAL mode. One DB at `~/.corral/corral.db`.
6. **API: HTTP + JSON over unix socket** (`~/.corral/corral.sock`, mode 0600). Remote access is opt-in: TCP listener with bearer token + TLS. No auth-less network exposure, ever.
7. **Isolation: one git worktree per concurrent task** (optional per-session flag), so parallel agents never fight over the working tree.

### Session state machine

```
            spawn                    hook: PreToolUse/…
  Created ────────► Starting ───────► Working ◄──────────┐
                                        │                 │
                     hook: Notification │ hook: Stop      │ answer/prompt
                     (permission/input) ▼                 │ delivered
                                      Blocked(reason) ────┘
                                        │
                     idle > N min       ▼
  Checkpointed ◄──────────────────── Idle
        │  wake / task / answer
        └────────► (resume) ────────► Working
  any state ── process exit ──► Dead (exit code, resumable?)
```

`Blocked(reason)` carries structured payload: permission request (tool + input), user question (AskUserQuestion options), plan approval, API error. This is the core product moment — the notification the user gets must say exactly what's needed and accept a one-line answer.

## 3. Repo layout

```
corral/
├── RUNBOOK.md
├── go.mod
├── cmd/corral/main.go            # single entrypoint, subcommand dispatch
├── internal/
│   ├── daemon/                   # lifecycle, API server, wiring
│   ├── supervisor/               # spawn, PTY, headless runner, idle reaper
│   ├── state/                    # state machine, hook event ingest
│   ├── checkpoint/               # session registry, resume logic
│   ├── orchestrator/             # task DAG, deps, retries
│   ├── notify/                   # ntfy/webhook/pushover backends
│   ├── store/                    # SQLite, migrations
│   ├── api/                      # HTTP handlers, client SDK (used by CLI)
│   ├── hookrelay/                # the hook-side helper logic
│   └── claude/                   # everything Claude-Code-specific:
│       ├── streamjson/           #   stream-json parser (versioned, golden-tested)
│       ├── sessions/             #   ~/.claude/projects layout, session-id discovery
│       └── settings/             #   hook injection into settings files
├── testdata/
│   ├── golden/streamjson/        # captured real outputs, per claude version
│   └── scenarios/                # fakeclaude scripts (see §5)
├── test/
│   ├── fakeclaude/               # mock claude binary (see §5)
│   ├── integration/
│   └── e2e/
└── .github/workflows/ci.yml
```

Dependencies (keep the list short on purpose): `creack/pty`, `modernc.org/sqlite`, `spf13/cobra` (or stdlib `flag` if you prefer zero deps), stdlib `net/http`. Nothing else without a written reason.

## 4. Milestones

Each milestone ends with: tests green, `README` section updated, demo command documented. Don't start M(n+1) with M(n) red.

### M0 — Spike (throwaway allowed)
Prove the two risky primitives before building anything real:
- Spawn `claude` in a PTY from a detached process; attach/detach from another terminal; TUI renders correctly (winsize, SIGWINCH).
- Kill a `claude` process mid-conversation; relaunch with `--resume <id>`; conversation continues.
- Capture real `stream-json` output for the golden corpus while you're at it.

**Exit criteria:** both primitives demonstrated in a script; findings written into `docs/spike-notes.md` (quirks, escape sequences, resume edge cases).

### M1 — Daemon skeleton + interactive sessions

> **Spike findings folded in (see docs/spike-notes.md):** (a) reattach requires a headless terminal emulator tracking screen-grid state per session — raw byte-ring replay corrupts alt-screen TUIs (proven with vi and claude); (b) daemonization = setsid re-exec pattern; (c) child env is a minimal explicit whitelist, never `os.Environ()` passthrough; (d) detach = tmux-style prefix key + socket goodbye frame so the daemon can tell detach from client crash; (e) every supervised session gets a pinned settings file — global user settings leak into headless behavior.

- `corral daemon` with unix-socket HTTP API; `corral ls / new / attach / kill`.
- Supervisor owns PTYs; attach/detach tmux-style; sessions survive client disconnect and daemon keeps them across its own restart (re-adopt or resume).
- SQLite store with migrations; structured logging (`log/slog`).

**Exit criteria:** close your laptop lid analog — kill the attached client, reattach, session intact. Integration tests cover spawn/attach/detach/kill.

### M2 — Hooks state engine (the moat)
- Hook injection on session create; `corral hook-relay` posting to the socket.
- State machine driven purely by hook events; `corral ls` shows `working / blocked(why) / idle`.
- Notifier v1: ntfy.sh + generic webhook. Notification includes the structured reason.
- `corral answer <session> "text"` delivers a reply into the session (PTY write for interactive; stream-json message for headless).

**Exit criteria:** run a real agent, walk away, get a phone notification "corral/api-refactor blocked: permission to run `npm test`", answer from the phone via ntfy reply → agent continues. This is the demo that sells the product — record it (asciinema + phone screen capture) for the README and launch post.

### M3 — Checkpoint/resume + idle reaping

> **Spike finding (M0, claude 2.1.224):** the session `.jsonl` is NOT flushed incrementally — a SIGKILL between "user message sent" and the post-turn flush loses the entire in-flight turn *cleanly* (no corruption, resume works, but the model has zero memory of the turn — even if the API had fully completed and streamed the answer to stdout). Consequences baked into this milestone: (a) corral tees and persists the CLI's stdout stream itself as the authoritative record of in-flight turns — never trusts the session file for the current turn; (b) the reaper checkpoints only at turn boundaries it has *observed flushed* (session file contains the matching assistant record), never mid-turn; (c) crash recovery compares corral's own stream log against the session file and re-sends the last prompt when the assistant record is missing, accepting the regeneration cost. Also: pin a known settings file for supervised sessions — permission behavior in `-p` is environment-dependent (global user settings leak into headless mode).

- Idle reaper (configurable timeout) kills idle sessions after checkpointing.
- `corral wake`, auto-wake on incoming answer/task.
- Daemon crash recovery: SIGKILL the daemon, restart, all sessions restored to correct state (running ones re-adopted or resumed, checkpointed ones stay checkpointed).

**Exit criteria:** chaos tests (§5) green. A machine reboot loses nothing but in-flight turns.

### M4 — Headless orchestration
- Task model: `corral run "prompt" --repo X --worktree --depends-on <task>`; DAG execution, retries with backoff, per-task worktree isolation.
- stream-json runner: parse turns, surface tool use, capture result + cost.
- Agents spawning agents: expose a scoped API token per session so an agent can `corral run` its own subtasks.
- Cost accounting: record per-turn usage from stream-json; `corral ls --cost`; per-task and per-DAG budgets with stop-at-limit.
- Model/effort tiering per DAG node: each task declares (or inherits) `--model` and effort — cheap models for mechanical stages, big models for hard ones. The orchestrator is the only layer that can make this call.
- Session hygiene: per-turn usage reveals bloated contexts; corral flags them and can trigger compaction or restart-with-summary as a task policy.
- Quota-aware scheduling: an always-on fleet lives inside subscription rate-limit windows (5-hour / weekly caps) — N agents against ONE quota. The orchestrator tracks per-window usage from stream-json, prioritizes interactive sessions over background tasks, schedules low-priority DAG nodes into fresh windows, and pauses/resumes the fleet at configurable thresholds instead of slamming into 429s mid-DAG. Optional burst-to-API: overflow to a metered API key when the window is exhausted, gated by the task's budget. No competitor handles this; it is the #1 operational pain of running many agents on one plan.
- `corral mcp`: the daemon exposed as an MCP server, so in-session agents get structured tools (`corral_status`, `corral_run`, `corral_wait`) with the same scoped per-session token — discoverable and typed, no shelling out and parsing CLI output. The CLI remains for humans and scripts.
- Review gate: task outputs stay on branches in their worktrees; nothing merges or pushes without human action. `corral review` lists pending diffs; the M5 dashboard renders them with one-tap approve.
- Auto-answer policy engine v1: rules over `Blocked` events — auto-approve allowlisted tools/commands per repo (`.corral.toml`), escalate everything else to the notifier. Default deny; rules are additive allowlists only; every auto-answer is audit-logged. This is the second moat: corral receives permission requests structured, so it can classify them — a scraper can't.

**Exit criteria:** a 3-node DAG (plan → implement → review) runs unattended with fakeclaude; with real claude behind an env-var gate.

### M5 — Remote access + dashboard
- Opt-in TCP listener: bearer token, TLS (self-signed bootstrap or tailscale-friendly).
- Minimal web dashboard: session list with live states, blocked-reason cards with answer box, task DAG view. Server-sent events for live updates.
- `corral` client `--host` flag to drive a remote daemon.

**Exit criteria:** manage a session on a home server from a phone browser.

### M6 — Learning loop (working name: hermes)

Thesis: every supervised session is an experiment; the runtime that observes all of them can compound their lessons into durable per-repo artifacts. Generating skills with an LLM is table stakes (`headroom learn`, skill-creator already exist) — the differentiator is **closing the loop with verification and measured adoption**, which only the runtime owner can do: corral sees full trajectories (hooks + stream-json + task outcomes + cost + corrections), can spawn cheap headless sessions to verify candidates, and can measure effect after adoption.

Pipeline — every stage event-sourced, every artifact provenance-tagged:

1. **Mine** the event store: repeated permission approvals, recurring command sequences, recurring failures/retries, user corrections mid-session, cost outliers, blocked-event patterns.
2. **Synthesize** candidate artifacts (cheap model, structured output): policy rules, `CLAUDE.local.md` entries, `SKILL.md` drafts, hook configs, model-routing hints.
3. **Verify** before proposing: re-run historical tasks headless with/without the candidate (fakeclaude for plumbing; real claude behind the E2E gate); compare success, cost, blocked count. No measured improvement → discarded, never shown.
4. **Propose, never write silently**: PR-style diff via `corral learnings` / dashboard, one-tap adopt/reject. Only trivial-risk classes (permission suggestions) may auto-adopt, and only if the user opts in per repo.
5. **Measure & decay**: adopted artifacts tracked against their promised metric; regression → auto-flag for rollback. Every artifact carries provenance (which sessions taught it) and a TTL — unused or stale artifacts get re-verified or retired. Bad learned rules compound too; the negative flywheel is this system's primary failure mode and every stage above is a brake on it.

Attack order (hardest ground truth first):

1. Policy suggestions — "you approved `npm test` 12× in this repo; allowlist it?" Perfect ground truth, immediate value.
2. Operational memory — SessionStart hook injects distilled repo facts (setup commands, flaky tests, quirks) from prior sessions.
3. Model-routing bandit — learn which tier suffices per task type per repo from cost/outcome history; feeds M4 tiering defaults.
4. Skill synthesis — recurring multi-step procedures → draft `SKILL.md`, sandbox-verified before proposal.
5. Failure regression corpus — every failed task saved as a replayable scenario; `corral regress` re-runs the corpus after any config/skill change. "CI for your agent setup" — standalone sellable feature.

Explicitly out of scope: fine-tuning, unsupervised self-modification, cross-user telemetry. Team-shared learnings ("fleet memory sync") is the natural paid tier; single-user OSS stays complete.

**Exit criteria:** on a dogfooded repo over 2 weeks, measurable reduction in blocked-events and cost versus the prior 2 weeks, with zero unapproved repo writes.

### Post-M6 backlog (unordered)
Windows support, multi-user/team mode (shared fleet learnings — the paid tier), Slack/Telegram notifier, session templates, Agent-SDK-based runner as alternative to CLI subprocess, claude version pinning per repo (`corral doctor` flags drift).

---

**This runbook is feature-complete for the v1 thesis.** Further additions before code exist are scope risk, not vision. The next information that should change this document comes from the M0 spike, not from more planning.

## 5. Test harness

The centerpiece is **fakeclaude** — without it every test needs an API key and burns money/minutes.

### 5.1 fakeclaude (mock agent binary)

A small Go binary (`test/fakeclaude`) that impersonates the `claude` CLI closely enough for corral to be fooled:

- Accepts the real flag surface corral uses: `-p`, `--resume`, `--output-format stream-json`, `--input-format stream-json`, `--settings`.
- **Script-driven:** `CORRAL_FAKE_SCENARIO=testdata/scenarios/blocked_permission.yaml` selects a scenario. A scenario is a list of steps:

```yaml
# testdata/scenarios/blocked_permission.yaml
steps:
  - emit: stream-json      # emit canned assistant/tool events
    file: turns/edit_file.jsonl
  - fire_hook: PreToolUse  # POST canned hook payload to $CORRAL_SOCK
    payload: hooks/pretooluse_bash.json
  - wait_for: stdin        # block until an answer arrives (stream-json or PTY line)
    timeout: 10s
  - fire_hook: Stop
  - write_session: true    # write a plausible ~/.claude/projects/<hash>/<id>.jsonl
  - exit: 0
```

- Simulates the session-file side effects (`--resume` finds and appends to the fake session file) so checkpoint/resume paths are testable.
- Interactive mode: renders a trivial prompt/echo TUI so PTY attach tests have something to assert against.
- Misbehavior scenarios: garbage output, hook never fires, hang forever, exit mid-turn, unknown stream-json event types. Corral must degrade gracefully (timeout → mark degraded, never crash the daemon).

### 5.2 Test layers

| Layer | What | Runs |
|---|---|---|
| Unit | state machine transitions (table-driven, every event × every state), stream-json parser vs golden files, settings/hook injection, store queries | every push, `-race` |
| Integration | real daemon + fakeclaude over real unix socket + real PTYs. Full lifecycles: spawn→work→block→answer→stop; checkpoint→resume; DAG execution | every push, `-race` |
| Chaos | SIGKILL daemon mid-turn → restart → assert recovery; SIGKILL agent → assert Dead+resumable; disk-full on checkpoint; socket flooding; clock skew on idle reaper | every push |
| Golden/compat | recorded real-claude outputs per version in `testdata/golden/`; parser must handle all; a `make record-golden` target regenerates against the installed claude (run on claude upgrades) | every push (parse), manual (record) |
| E2E (real claude) | one cheap real task ("create hello.txt") through the full stack, gated by `CORRAL_E2E=1` + API key | manual / nightly |

### 5.3 Hardening layers (required from the milestone noted)

| Layer | What | From |
|---|---|---|
| Contract tests (mock fidelity) | The same scenario assertions run against **both** fakeclaude and real claude (`CORRAL_CONTRACT=1`, nightly + on claude upgrades). Any divergence = fakeclaude is lying; fixing the mock blocks everything else. Without this, the whole harness can be green against a fiction. | M2 |
| Fake clock | All timeout/reaper logic takes an injected `Clock` interface; tests advance time synthetically. No `time.Sleep`-driven assertions, no flaky timing tests. | M1 |
| Fuzzing | Go native fuzzing on the stream-json parser and hook-payload decoder — they consume output of an external process, treat it as untrusted. Corpus seeded from golden files. | M2 |
| Stress | 50 concurrent fakeclaude sessions × event floods through one daemon: assert no deadlock, no lost/misordered events per session, API stays responsive. `-race` on. | M3 |
| Soak + leak detection | Nightly: spawn/kill/resume 1000 sessions sequentially; assert open-fd count and RSS return to baseline (PTYs leak fds notoriously), SQLite file size bounded (WAL checkpointing works). | M3 |
| Migration tests | Every schema migration ships with a test: populated DB at version N-1 → open with N → data intact. Keep fixture DBs per released version in `testdata/db/`. | M1 (first migration) |
| Notifier tests | In-process mock ntfy/webhook server: assert payload shape, delivery retry with backoff, and that a failed notification never blocks the state machine. | M2 |
| Security tests | §7 promises as executable tests: socket file mode is 0600; hook event without the per-session secret → rejected; child-agent scoped token cannot touch sessions outside its subtree; `corral answer` payload with shell metacharacters arrives verbatim, uninterpreted. | each item lands with its milestone |

Flakiness policy: a test that flakes twice gets quarantined (skipped with a tracking issue) the same day — a suite people retry is a suite people ignore. No `time.Sleep` synchronization in tests; wait on events/channels with the fake clock.

### 5.4 Invariants to assert everywhere

- Daemon never crashes because of anything an agent process does.
- Every session in the store is always in exactly one state; every transition is event-sourced (append-only `events` table) so `corral log <session>` can replay history.
- Killing corral (daemon or client) never corrupts a Claude session — the underlying `~/.claude` data is treated as read-mostly and sacred.
- No test talks to the network (except gated E2E). `go test ./...` works on a plane.

### 5.5 CI (GitHub Actions)

- Matrix: `macos-latest`, `ubuntu-latest`.
- Steps: `go vet`, `golangci-lint`, `go test -race ./...`, `go build` (CGO_ENABLED=0), integration + chaos suites, artifact upload of the binary.
- PTY tests need a TTY-less-safe design: integration tests create PTYs themselves (they don't need the CI runner to have one).
- Nightly job (optional): E2E with real claude against a scratch repo.

## 6. Observability & ops

- `log/slog` JSON logs to `~/.corral/log/daemon.jsonl`, level via config.
- `corral doctor`: checks socket, DB integrity, claude binary presence/version, hook wiring in a given repo, dangling sessions.
- Event-sourced `events` table doubles as an audit log; `corral log <session>` replays it.
- Metrics later (Prometheus endpoint on the TCP listener), not before M5.

## 7. Security baseline

Applies from M1, not bolted on later:

- Unix socket `0600`, owned by the user. No TCP by default.
- TCP (M5) requires a bearer token generated by `corral token create`; TLS mandatory; tokens hashed at rest; per-session scoped tokens for agent-spawned-agent so a child agent can only manage its own subtree.
- Hook relay authenticates to the daemon with a per-session secret injected at spawn time (env var), so arbitrary local processes can't forge state events.
- `corral answer` payloads are delivered as data, never shell-interpolated.
- Secrets (notifier tokens, API tokens) in `~/.corral/config.toml` mode 0600; never logged.
- Event ingest runs a secret scan (common token/key/credential patterns) and redacts before persisting — the event store is append-only and long-lived; storing leaked credentials forever is a liability corral must not create. Redactions are marked in place, never silent.

## 8. Language decision record (Go over Rust/C)

- Workload: process supervision, PTYs, JSON, sockets — I/O-bound; the LLM is the latency floor. Rust/C performance buys nothing measurable.
- The competitive variable is iteration speed against a fast-moving target (Claude Code changes weekly) and an established YC competitor. Go's compile speed, tiny language surface, and goroutine-per-agent model maximize it.
- Static single binary + trivial cross-compile (CGO_ENABLED=0, pure-Go SQLite) preserves the "one curl install" distribution story.
- Revisit trigger: if a component ever needs zero-GC determinism or heavy in-process compute (unlikely), isolate it — don't rewrite the product.

## 9. Risks

| Risk | Mitigation |
|---|---|
| Claude Code changes flags/formats/hooks | `internal/claude/` isolates ALL claude-specific knowledge; golden corpus per version; `corral doctor` detects version drift; CI nightly against latest claude |
| Anthropic ships this natively (web/cloud sessions already close) | Corral's wedge is self-hosted + your own hardware + orchestration; keep the daemon thin so pivoting to Agent SDK runner is cheap |
| `--resume` semantics change or session files become opaque | Never write to `~/.claude` session files; only read IDs and pass them back to the CLI; headless runner (stream-json) is the fallback persistence path |
| PTY edge cases (resize, colors, alt-screen) eat weeks | M0 spike de-risks first; attach UX can lag feature-wise (tmux exists) — hooks/orchestration are the product, not terminal emulation |
| Scope creep toward Herdr's 19 agents | Say no. The runbook thesis is depth. Multi-agent = separate product decision with its own runbook |
| Learning loop poisons itself (bad rules compound) | Verify-before-propose, consented adoption, measured effect with auto-flag rollback, provenance + TTL on every artifact (M6 pipeline is designed around this failure mode) |

## 10. Cross-cutting requirements

Things that belong to no single milestone but must not be improvised late:

- **Configuration:** `~/.corral/config.toml` (user) + `.corral.toml` (per-repo) + env var overrides, in that precedence order. `corral config` prints the effective merged config with each value's source. Documented schema from M1.
- **API versioning & version skew:** all endpoints under `/v1/`; client↔daemon handshake exchanges versions on connect — mismatch fails with a clear message, never undefined behavior. Daemon upgrade with live sessions has a defined path: checkpoint-all → swap binary → resume-all, covered by an integration test.
- **Packaging & service install:** goreleaser (darwin/linux × amd64/arm64), curl installer, brew tap, `corral service install` writes the launchd plist / systemd unit with restart-on-crash. macOS: codesign + notarize — an unsigned background daemon gets flagged by Gatekeeper and kills adoption on first contact.
- **Data retention:** the event-sourced `events` table and logs grow unbounded by design — prune by age + size (configurable), `corral gc` command, scheduled SQLite `VACUUM`/WAL checkpoint.
- **Resource limits & backpressure:** cap on concurrent live sessions (queue beyond it), per-session log size caps, disk-low behavior = checkpoint everything and refuse new spawns loudly.
- **Token economy — division of labor:** request-level compression (payload shrinking, cache alignment) is **out of scope permanently** — delegate to [headroom](https://github.com/headroomlabs-ai/headroom) via composition: when enabled (config or `--headroom` flag), corral spawns sessions with `ANTHROPIC_BASE_URL` pointed at the local headroom proxy; corral is the policy point for which sessions get it. Corral owns only *orchestration-level* economy: model/effort tiering per task, budgets, compaction triggers (see M4). Rebuilding compression would mean competing with a 65k-star dedicated product while fighting Herdr — the risk table's scope-creep rule applies.
- **Decisions to record as ADRs (`docs/adr/`) before M1:** license (Apache-2.0 matches the ecosystem and Herdr — being *more* restrictive than the incumbent is a handicap), telemetry (recommendation: none, or opt-in crash reports only — "self-hosted and silent" is part of the pitch), final name + GitHub org / domain availability check. §8 of this runbook becomes ADR-0001.

## 11. Working agreements

- Conventional commits; branch per milestone task; PR-sized changes even solo.
- Every bug found manually becomes a scenario in `testdata/scenarios/` before the fix lands.
- `internal/claude/` is the only package allowed to know Claude Code exists; everything else speaks corral's own types. This keeps the (unlikely) multi-agent pivot possible without betting on it.
- Cut a tagged release at each milestone; `corral --version` from day one.
- Dogfood from M2 onward: corral itself is developed inside corral-supervised sessions. Every pain point felt while dogfooding outranks the backlog.
