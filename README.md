# corral

> Working name. The runtime your Claude Code agents live on — supervised, resumable, orchestrated, and learning from every session.

A daemon that supervises Claude Code sessions: knows their exact state via hooks (never terminal scraping), checkpoints idle sessions to disk and resumes them on demand, orchestrates task DAGs with per-task model tiering and budgets, and answers you on your phone when an agent is blocked.

**Status: pre-alpha — M5 complete (`v0.5.0`)**. corral now includes supervised interactive sessions, checkpoint/resume and idle reaping, headless task-DAG orchestration, and opt-in remote access through TLS + bearer tokens with a live web dashboard. See [RUNBOOK.md](RUNBOOK.md) for the full architecture and roadmap; [docs/design/m5.md](docs/design/m5.md) for the completed M5 design.

## Try it

```sh
go build -o corral ./cmd/corral

./corral daemon                # starts the background daemon
./corral new --cwd ~/code/x    # spawns a supervised claude session and attaches
# … work with claude normally; detach with Ctrl-\ then d …
./corral ls                    # NAME STATE ATTACHED CWD UPTIME PID
./corral attach x-1            # reattach — full screen repaint, state intact
./corral kill x-1
```

Kill the terminal mid-session and reattach: the session is intact. Restart the daemon: sessions come back via `claude --resume` (in-flight turns survive graceful shutdown as partial-but-marked; see `spike/verify/NOTES.md` V3).

## Layout

- `RUNBOOK.md` — architecture, milestones M0–M6, test harness design. The source of truth.
- `docs/adr/` — architecture decision records.
- `spike/` — M0 throwaway spikes (PTY ownership, kill/resume semantics). Code is disposable; the `NOTES.md` files are the deliverable.
- `testdata/golden/streamjson/` — captured real `claude` stream-json transcripts, versioned; the parser's golden corpus.
