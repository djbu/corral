# Step 35 implementer brief — E2E of the M5 exit criterion (scripted remote)

> **Status:** completed and verified for `v0.5.0`. Retained as a historical implementation brief;
> its coordinator-only git restrictions applied while the E2E was being implemented.

You are implementing step 35 of corral's M5 milestone — the FINAL step. Module root:
`/Users/danielbecerra/code/research/corral`. Branch `spike/m0`. Go single-binary daemon.

Goal (m5.md §14 "E2E" + §15 step 35): a scripted end-to-end test proving the literal exit
criterion — "manage a session on a home server from a phone browser": a real daemon with the
TCP+TLS listener on and one token minted; a remote client over TLS+bearer sees a session go
`blocked` live over SSE, submits an answer, and observes the session unblock. The scripted-HTTP
leg ALWAYS runs. The real-browser leg is NOT implemented (see §"Browser leg" below).

## HARD RULES (non-negotiable)
- **You are FORBIDDEN from running ANY git-mutating command** (commit/add/tag/checkout/branch/
  stash/reset/push/rm/merge/worktree/init). **In particular DO NOT create the `v0.5.0` tag** —
  the coordinator tags personally after verification. The ONLY git allowed is throwaway repos
  under `t.TempDir()` inside tests.
- Do NOT write to the real `~/.claude` or real `~/.corral`. Use temp HOME/state dirs.
- NEVER use `InsecureSkipVerify` or `--insecure`. The test pins the daemon's self-signed CA.
- Match surrounding test style exactly. READ the neighbor files listed below before writing.
- When done, report files touched + deviations + PASTED real output of the commands in the
  "Deliverable" section. Do not declare success without pasted output.

## READ FIRST (do not skip — these are the idiom you must match)
1. `cmd/corral/daemon_e2e_test.go` — canonical subprocess-daemon boot. Shared helpers you MUST
   reuse: `mustGetwd`, `waitForVersion(ctx, t, c)`, `waitForGone(t, path, timeout)`. Note the
   AF_UNIX `sun_path` ~104-byte limit → socket dir via `os.MkdirTemp("", "corral-e2e-")` +
   `t.Cleanup(os.RemoveAll)`, NOT `t.TempDir()`. `state_dir`/cwd may use `t.TempDir()`.
2. `cmd/corral/session_lifecycle_e2e_test.go` — full CLI→daemon→supervisor→fakeclaude loop;
   `buildFakeClaudeE2E(t, dir)` builds the fake claude binary; `-short` skip guard at top.
3. `internal/api/answer_e2e_test.go` — **THE crux reference.** It already drives the full
   PermissionRequest→blocked→answer→PTY→resolve cycle in-process with a real fakeclaude
   scenario (wait_stdin step + `fire_hook PostToolUse` resolve step). Study exactly how it
   defines/loads the fakeclaude scenario and how the answer reaching the PTY causes fakeclaude
   to fire the resolving hook. You are lifting that scenario into a subprocess-daemon + TLS + SSE
   test. Whatever env var / scenario-file mechanism it uses to hand fakeclaude its script, reuse
   the same mechanism (it flows through the session's env).
4. `test/fakeclaude/` — the fake claude binary + its scenario format/step vocabulary.
5. `internal/api/client/client.go` (`New`, `NewRemote`, `Version`, unexported `do`),
   `client/sessions.go` (`CreateSession`, `GetSession`, `Answer`, `SessionInfo.AgentState`),
   `client/tokens.go` (`CreateToken`, `CreateTokenRequest{Label,Scope,SessionID}`, `TokenCreated{Token}`).
6. `internal/daemon/daemon.go` around :416-454 (TCP+TLS wiring) and :505 (AuthenticatedHandler),
   `internal/tlsbootstrap/tlsbootstrap.go` (writes `<state_dir>/tls/cert.pem`).
7. `internal/config/env.go` — env var names: `CORRAL_DAEMON_LISTEN`, `CORRAL_DAEMON_SOCKET`,
   `CORRAL_DAEMON_STATE_DIR`, `CORRAL_SESSION_CLAUDE_BIN`, `CORRAL_STATE_PERMISSION_SETTLE`.

## Test file
New file `cmd/corral/dashboard_remote_e2e_test.go`, package `main` (same as the other e2e tests),
top-level `-short` skip. One top-level `TestDashboardRemoteE2E` with subtests.

## Harness construction (write a helper `startRemoteDaemon(t)` returning what the subtests need)
1. **Pre-select a free port** (do NOT parse the daemon log): bind `net.Listen("tcp","127.0.0.1:0")`,
   read `.Addr().String()`, `Close()`, keep the `127.0.0.1:PORT` string. Accept the tiny
   bind-close-rebind race (standard idiom).
2. socket dir via `os.MkdirTemp` (AF_UNIX limit); `stateDir := t.TempDir()`; build fakeclaude via
   `buildFakeClaudeE2E`.
3. Boot the daemon subprocess exactly as `session_lifecycle_e2e_test.go` does, but ALSO set:
   - `CORRAL_DAEMON_LISTEN=127.0.0.1:PORT`
   - `CORRAL_STATE_PERMISSION_SETTLE=200ms` (so `blocked` is reached in a fraction of a second on
     the real clock instead of the 15s default — this is the key to a fast test)
   - leave `CORRAL_DAEMON_TLS_CERT`/`KEY` UNSET so the daemon self-signs into `<stateDir>/tls/cert.pem`.
4. `unixC := client.New(sockPath, os.Stderr)`; `waitForVersion` on it for readiness.
5. Mint an ADMIN token over the token-free unix socket:
   `tc, _ := unixC.CreateToken(ctx, client.CreateTokenRequest{Label:"e2e", Scope:"admin"})` →
   `tc.Token` is the plaintext. (This is the clean path — the socket needs no auth.)
6. `caPath := filepath.Join(stateDir, "tls", "cert.pem")`. Poll until it exists (the daemon writes
   it during TCP bring-up; it may lag the unix `Version` readiness — poll with a short deadline).
7. `remote, err := client.NewRemote("127.0.0.1:PORT", tc.Token, caPath, os.Stderr)`.
8. `t.Cleanup`: `unixC.Shutdown(ctx, 0)` + `waitForGone(sockPath)`.

## SSE + dashboard: no client methods exist — use raw net/http
- Build an `*http.Client` whose `Transport.TLSClientConfig.RootCAs` is a pool loaded from `caPath`
  (NOT InsecureSkipVerify). Reuse the CA-pinning approach `NewRemote` uses internally — read it and
  mirror it; do not invent a laxer one.
- SSE: `GET https://127.0.0.1:PORT/v1/events/stream` — EventSource-style. Auth via the `?token=`
  query-param exemption (see `middleware_auth.go` ~:74-85; the version handshake is also exempted
  for this one route). Read the response body line-by-line; a `data:`-prefixed line = one frame.
  Run the reader in a goroutine feeding a channel; the test asserts "≥1 frame arrived within T".
  **Frames are invalidation signals only — do NOT parse their `kind`/content** (that is the
  step-32/33 dashboard contract). Frame arrival + a subsequent snapshot refetch is the assertion.
- Dashboard snapshot: `GET https://127.0.0.1:PORT/v1/dashboard` with `Authorization: Bearer <token>`
  AND `Corral-Api-Version: 1` header. Decode `{sessions:[...], dags:[...]}`; find the session and
  assert `agent_state`. This is the exact byte path the phone uses — assert it explicitly, distinct
  from `GetSession`.

## The scripted-HTTP scenario (subtest `AdminPath`)
1. Subscribe SSE (start the reader goroutine) BEFORE driving to blocked.
2. `CreateSession` via `remote` with the fakeclaude scenario that: starts working → fires a
   `PermissionRequest` hook (→ after the 200ms settle the engine flips to `blocked`) → `wait_stdin`
   → on receiving the answer bytes, fires a resolving hook (`PostToolUse`) → finishes. (Model the
   scenario EXACTLY on `answer_e2e_test.go`; that is the proven recipe.)
3. Poll `remote.GetSession(id).AgentState` until `== "blocked"` (deadline ~5s). Assert ≥1 SSE frame
   arrived by now.
4. Assert `GET /v1/dashboard` (raw pinned-CA bearer client) lists the session with
   `agent_state == "blocked"`.
5. `remote.Answer(ctx, id, "yes", "", true)` (check the real `Answer` signature — key/newline args).
6. Poll `GetSession(id).AgentState` until it LEAVES `blocked` (→ working/idle) within a deadline.
   Assert another SSE frame arrived across the answer. This is the "observe unblock" step.
7. Fail with clear messages on any timeout (dump last AgentState + frames-seen count).

## Scoped-token subtest (`ScopedPath`) — the advisor-flagged must-have
- Mint a `scope='session'` token rooted at the session id from the admin path (or a fresh session):
  `unixC.CreateToken(ctx, {Label:"e2e-scoped", Scope:"session", SessionID:<sessionID>})`.
- `NewRemote` with that token. Assert it CAN run the same blocked→answer→unblock cycle on ITS OWN
  session (own-session is always in its subtree, even with no task backing — this proves step-34
  enforcement doesn't over-block the legitimate self case).
- Create a SECOND session; assert the scoped token gets **403** on `GetSession`/`Answer`/dashboard-
  filtered-out for that foreign session (proves confinement holds at the integration level, not
  just unit level). For the dashboard, assert the foreign session is ABSENT from the scoped token's
  `/v1/dashboard` snapshot.

## Browser leg — DO NOT implement; record as deferred
No browser-automation dependency exists in the tree (no chromedp/playwright/rod) and adding one for
a gated test is out of scope. In `docs/design/m5.md` §16 (Deferred), add ONE bullet:
"Real-browser E2E leg (`CORRAL_E2E_BROWSER=1`, chrome automation) deferred — no browser dependency
in the tree; the scripted-HTTP E2E (`cmd/corral/dashboard_remote_e2e_test.go`) exercises the same
end-to-end path (TLS + bearer → dashboard snapshot → live SSE → answer → unblock)." Do not add any
`CORRAL_E2E_BROWSER` code.

## Deliverable (paste REAL output; do NOT tag)
Run and paste:
```
cd /Users/danielbecerra/code/research/corral
go build ./... && go vet ./...
gofmt -l cmd/corral/dashboard_remote_e2e_test.go docs/design/m5.md
go test -race ./cmd/corral/ -run TestDashboardRemoteE2E -v
go test -race ./...
```
Report: (1) files touched + one-line why; (2) any deviation from this brief + why (especially the
exact fakeclaude scenario mechanism you found in answer_e2e_test.go and how you reused it — flag if
it differs from what this brief assumed); (3) the pasted command output. If the blocked→unblock
cycle does not work over the subprocess daemon (e.g. the hook relay doesn't reach the engine, or
the scenario mechanism differs), STOP and report the blocker rather than forcing a weaker assertion.
Do NOT commit and do NOT tag.
```
