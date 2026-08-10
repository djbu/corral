# corral

> Working name. The runtime your Claude Code agents live on — supervised, resumable, orchestrated, and learning from every session.

[![CI](https://github.com/djbu/corral/actions/workflows/ci.yml/badge.svg)](https://github.com/djbu/corral/actions/workflows/ci.yml)

A daemon that supervises Claude Code sessions: knows their exact state via hooks (never terminal scraping), checkpoints idle sessions to disk and resumes them on demand, orchestrates task DAGs with per-task model tiering and budgets, and answers you on your phone when an agent is blocked.

**Status: pre-alpha — M6 complete (`v0.6.0`), M7 in progress.** corral now includes supervised interactive sessions, checkpoint/resume and idle reaping, headless task-DAG orchestration, opt-in remote access through TLS + bearer tokens, and a verified per-repository permission-learning loop. M7A established the private canonical repository and protected macOS/Linux CI; M7B adds reproducible private release artifacts; M7C adds safe daemon lifecycle commands and per-user launchd/systemd operation. Start with the human-facing [usage guide](docs/manual/GUIA_DE_USO.md), [architecture manual](docs/manual/ARQUITECTURA.md), or [private release runbook](docs/manual/RELEASES_PRIVADAS.md). The ordered work lives in the [post-M6 execution plan](docs/roadmap/POST_M6.md).

## Try it

```sh
go build -o corral ./cmd/corral

./corral daemon                # starts the background daemon
./corral daemon status
./corral daemon restart        # graceful checkpoint + recovery
./corral new --cwd ~/code/x    # spawns a supervised claude session and attaches
# … work with claude normally; detach with Ctrl-\ then d …
./corral ls                    # NAME STATE ATTACHED CWD UPTIME PID
./corral attach x-1            # reattach — full screen repaint, state intact
./corral kill x-1

# optional: run automatically in this user's login session
./corral service install       # launchd on macOS; systemd --user on Linux
./corral service status

# after repeated manual approvals in a repository:
./corral learnings scan --repo ~/code/x
./corral learnings list --repo ~/code/x --status proposed
./corral learnings show <id> --diff
./corral learnings adopt <id>
./corral learnings report <id>           # default: fixed 14-day post window
./corral learnings report <id> --early   # explicit close once samples suffice
```

Kill the terminal mid-session and reattach: the session is intact. Restart the daemon: sessions come back via `claude --resume` (in-flight turns survive graceful shutdown as partial-but-marked; see `spike/verify/NOTES.md` V3).

## Layout

- `docs/manual/` — human-facing architecture and usage manuals.
- `docs/roadmap/` — ordered execution plans for work after completed milestones.
- `RUNBOOK.md` — architecture, milestones M0–M6, test harness design. The source of truth.
- `docs/adr/` — architecture decision records.
- `LICENSE` — Apache License 2.0.
- `CONTRIBUTING.md` — contribution workflow and required checks.
- `spike/` — M0 throwaway spikes (PTY ownership, kill/resume semantics). Code is disposable; the `NOTES.md` files are the deliverable.
- `testdata/golden/streamjson/` — captured real `claude` stream-json transcripts, versioned; the parser's golden corpus.
