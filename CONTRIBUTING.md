# Contributing to corral

corral is currently developed in the private `djbu/corral` repository and is
pre-alpha. Every change should preserve the safety boundaries documented in
`RUNBOOK.md` and the current milestone design.

## Workflow

1. Branch from `main` using the `codex/` prefix for Codex-authored work.
2. Keep commits focused and use conventional commit messages.
3. Add a regression test for every bug fix.
4. Do not commit credentials, local state, captured private transcripts, or
   generated files from `~/.corral` and `~/.claude`.
5. Open a draft pull request until all required checks pass.

## Required checks

```sh
gofmt -w <changed-go-files>
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
node --check internal/api/dashboard/app.js
```

Tests that use a real Claude account remain opt-in and never replace the
deterministic suite. Document any gated test run and the Claude Code version in
the pull request.

## Security-sensitive changes

Authentication, tokens, permission rules, hook payloads, repository writes,
worktree deletion, and remote input require explicit threat-model coverage and
negative tests. Repository configuration is untrusted input and must not widen
operator policy.
