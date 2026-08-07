# corral

> Working name. The runtime your Claude Code agents live on — supervised, resumable, orchestrated, and learning from every session.

A daemon that supervises Claude Code sessions: knows their exact state via hooks (never terminal scraping), checkpoints idle sessions to disk and resumes them on demand, orchestrates task DAGs with per-task model tiering and budgets, and answers you on your phone when an agent is blocked.

**Status: pre-alpha — M0 spike in progress.** See [RUNBOOK.md](RUNBOOK.md) for the full architecture, milestones, and test strategy.

## Layout

- `RUNBOOK.md` — architecture, milestones M0–M6, test harness design. The source of truth.
- `docs/adr/` — architecture decision records.
- `spike/` — M0 throwaway spikes (PTY ownership, kill/resume semantics). Code is disposable; the `NOTES.md` files are the deliverable.
- `testdata/golden/streamjson/` — captured real `claude` stream-json transcripts, versioned; the parser's golden corpus.
