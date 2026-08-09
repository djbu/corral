# corral — M5 completion handoff

> **Status update:** M5 step 35 has been implemented and verified. The scripted remote E2E lives
> in `cmd/corral/dashboard_remote_e2e_test.go`; `go build ./...`, `go vet ./...`, the targeted race
> test, and `go test -race ./...` are green. This document is retained as the implementation record.

You are picking up an in-progress Go project called **corral** and continuing its roadmap. This
document is fully self-contained: assume you have no prior context, no memory of earlier work, and
no access to any external advisor. Read it top to bottom before acting.

---

## 1. What corral is

- A **single-binary Go daemon** (`github.com/danielbecerra/corral`) that supervises Claude Code CLI
  sessions. Module root on the author's machine: `/Users/danielbecerra/code/research/corral`.
- Architecture: a background **daemon** process holds session state in a SQLite-backed `store`,
  supervises child `claude` processes over PTYs, and exposes an HTTP API on **two** transports:
  1. a local **unix socket** (0600, no auth — this is how local CLI subcommands and child agents
     talk to the daemon), and
  2. an opt-in **TCP+TLS** listener (bearer-token auth, self-signed cert) for remote access.
- A **web dashboard** (served on the TCP handler only) lets you manage sessions from a browser.
- Sessions can be organized into **DAGs** of tasks (headless orchestration).
- Development is on git branch **`spike/m0`**. There is **NO git remote** — you cannot push; commits
  and tags are local only.

### Key security invariants (do NOT violate — these are load-bearing)
- TCP requires a bearer token; TLS is mandatory (no plaintext-TCP path). Tokens hashed at rest
  (SHA-256, `crl_` prefix). Unix socket stays 0600 with no TCP by default.
- **Never** use `InsecureSkipVerify` or add an `--insecure` flag. Remote clients pin the daemon's
  self-signed CA cert.
- Two token scopes: `admin` (full access) and `session` (confined to one session's subtree). A
  `scope='session'` token presented over TCP may only touch its rooted session + that session's
  `task_deps` descendants within the same `dag_id`; every other target → 403. Admin tokens and
  absent-token (unix-socket) callers bypass this entirely. This enforcement shipped in step 34.
- Per-session secrets are never persisted/logged/returned by the API (in-memory only). corral
  **never** merges or pushes git; `corral review` is read-only. Never write to the real
  `~/.claude` or `~/.corral` — tests use temp/fake dirs.

---

## 2. Roadmap status

The project completed **milestone M5 = "Remote access + dashboard"**. Exit criterion: *"manage a
session on a home server from a phone browser."* Design doc: `docs/design/m5.md` (its §15 lists the
completed step plan, steps 26–35). The milestone closes with the annotated `v0.5.0` tag.

| Step | Description | Status |
|------|-------------|--------|
| 26 | token store + mint/hash/verify | ✅ done |
| 27 | `corral token` CLI + `/v1/tokens` endpoints | ✅ done |
| 28 | bearer-auth middleware | ✅ done |
| 29 | TLS self-signed bootstrap | ✅ done |
| 30 | opt-in TCP+TLS listener | ✅ done |
| 31 | remote client `--host` | ✅ done |
| 32 | SSE broker + `/v1/events/stream` | ✅ done |
| 33 | web dashboard | ✅ done |
| 34 | per-session scoped token **enforcement** | ✅ done (commit `c8ac9e9`) |
| 35 | **E2E + close v0.5.0** | ✅ done |

Note on step 34: the original design imagined the orchestrator *minting* a scoped token and
injecting it into child agents' env. That was **dropped** — child agents talk to the daemon over the
token-free unix socket, so a minted token would have no consumer. Step 34 therefore does
**enforcement only** (confine a `scope='session'` token when it *is* presented over TCP), mints
nothing, and touches neither `BuildEnv` nor the env whitelist. Confining a *local* child is a future
decision, not part of M5.

---

## 3. Step 35 implementation record

Deliverable (from `docs/design/m5.md` §15 step 35, verbatim): *"E2E + close — scripted-remote (and
gated real-browser) E2E of the phone-browser exit criterion (blocked→answer→unblock over
TLS+token+SSE); full `go test -race ./...`; tag `v0.5.0`."*

A **detailed implementer brief already exists** at `docs/design/step35-brief.md` — it names the exact
neighbor files to read, the test file to create, the harness construction, the assertions, and the
verification commands. **Follow that brief.** The summary below is the essence:

1. Create `cmd/corral/dashboard_remote_e2e_test.go` (package `main`, `-short` skip guard), one
   `TestDashboardRemoteE2E` with subtests.
2. Boot a **real daemon in a subprocess** with the TCP+TLS listener enabled and a low permission
   settle so `blocked` is reached fast:
   - env `CORRAL_DAEMON_LISTEN=127.0.0.1:PORT` (pre-select a free port by binding
     `127.0.0.1:0`, reading `.Addr()`, then closing — do NOT parse the daemon log),
   - env `CORRAL_STATE_PERMISSION_SETTLE=200ms`,
   - leave TLS cert/key env UNSET so the daemon self-signs into `<stateDir>/tls/cert.pem`,
   - socket dir via `os.MkdirTemp` (NOT `t.TempDir()` — AF_UNIX `sun_path` length limit),
   - reuse existing e2e helpers `mustGetwd`, `waitForVersion`, `waitForGone`, `buildFakeClaudeE2E`
     (see `cmd/corral/daemon_e2e_test.go` and `session_lifecycle_e2e_test.go`).
3. Mint an **admin token over the token-free unix socket** (`client.New(sock).CreateToken`), then
   build a remote client with `client.NewRemote("127.0.0.1:PORT", token, caPath, stderr)` where
   `caPath = <stateDir>/tls/cert.pem`.
4. **Drive blocked→answer→unblock** with a fakeclaude scenario modeled *exactly* on
   `internal/api/answer_e2e_test.go` — the proven recipe: fire `PermissionRequest` hook → after the
   settle timer the engine flips to `blocked` → fakeclaude `wait_stdin` → the `Answer` POST writes to
   the PTY → fakeclaude reads it and fires a **resolving hook** (`PostToolUse`) → session unblocks.
   The answer itself does NOT unblock; the resolving hook does. Study how the scenario is handed to
   the fake binary and reuse that same mechanism.
5. **Assert over the remote path:** subscribe SSE (`GET /v1/events/stream` via raw `net/http` with
   pinned CA; auth via `?token=` query exemption) BEFORE driving to blocked; assert ≥1 frame flows;
   assert `GetSession().AgentState == "blocked"`; assert `GET /v1/dashboard` (raw http, pinned CA,
   `Authorization: Bearer`, `Corral-Api-Version: 1` header) lists the session as blocked; call
   `Answer`; assert another frame flows and `AgentState` leaves blocked. **Do not parse SSE frame
   content** — frames are invalidation signals only; assert *frame arrival* + *snapshot state
   transition*.
6. **Scoped-token subtest:** mint a `scope='session'` token rooted at the session; assert it can run
   the same cycle on its OWN session (a non-task session's subtree is just `{itself}`, so own-session
   is allowed), and assert it gets **403** on a second, foreign session (and that the foreign session
   is absent from the scoped token's `/v1/dashboard` snapshot).
7. **Browser leg:** do NOT implement it. No browser-automation dependency exists in the tree and
   adding one for a gated test is out of scope. Instead add one bullet to `docs/design/m5.md` §16
   (Deferred): the real-browser leg is deferred; the scripted-HTTP E2E covers the same end-to-end
   path.

### Verify, commit, tag (all local)
Run and confirm clean:
```
cd /Users/danielbecerra/code/research/corral
go build ./... && go vet ./...
gofmt -l cmd/corral/dashboard_remote_e2e_test.go docs/design/m5.md
go test -race ./cmd/corral/ -run TestDashboardRemoteE2E -v
go test -race ./...            # full suite; step 34 left this at 811 passing
```
Then, and only after the suite is green:
```
git add -A
git commit -m "feat(m5): scripted-remote E2E of exit criterion (step 35)"   # NO Co-Authored-By line
git tag -a v0.5.0 -m "M5: remote access + dashboard"                        # annotated; matches v0.4.0 convention
```
(There is no remote, so no `git push`.)

---

## 4. Working conventions (author's standing preferences)
- After a change: conventional commit (`feat(...)`, `fix(...)`, etc). **No `Co-Authored-By` lines.**
- Milestone close = annotated git tag `vX.Y.0` (prior: `v0.4.0` "M4: headless orchestration").
- Match the surrounding code's style; read neighbor files before writing.
- If the blocked→unblock cycle cannot be made to work over the subprocess daemon (e.g. the hook
  relay doesn't reach the engine), STOP and report the blocker rather than weakening the assertion.

---

## 5. After step 35
M5 is complete once `v0.5.0` is tagged. The next milestone (M6) is defined in the project's
`RUNBOOK`; consult it for the following roadmap step. Do not start M6 without confirming scope with
the author.
