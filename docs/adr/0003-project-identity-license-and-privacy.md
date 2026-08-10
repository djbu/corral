# ADR-0003: Project identity, license, privacy, and telemetry

**Status:** Accepted — 2026-08-09

## Context

M0–M6 were developed locally under the provisional module path
`github.com/danielbecerra/corral`. M7 establishes a canonical GitHub home and a
legal and privacy baseline before CI, releases, installers, or collaborators
depend on those identities.

corral stores long-lived session, event, task, cost, and learning evidence. A
default telemetry channel would conflict with the self-hosted and silent
product position and would create a new data-exfiltration boundary.

## Decision

- The project name is `corral`.
- The canonical repository is `https://github.com/djbu/corral`.
- The Go module path is `github.com/djbu/corral`.
- The GitHub repository is private until the owner explicitly changes its
  visibility.
- Source code is licensed under Apache License 2.0 even while the repository is
  private, so contributions and any future distribution have unambiguous
  terms.
- corral sends no product telemetry, analytics, usage events, or crash reports.
  Network access occurs only for operator-configured product functions such as
  Claude, ntfy/webhooks, remote API clients, and gated tests.
- A future telemetry feature requires a new ADR, must be opt-in, and must expose
  the exact payload and destination before activation.

## Consequences

- All internal imports, build-time linker paths, tests, and documentation use
  `github.com/djbu/corral`.
- GitHub Actions and release assets are private and require authenticated
  access. A public Homebrew tap is deferred while the repository remains
  private.
- Moving the module again would be a breaking identity change and requires an
  explicit migration plan.
- The absence of telemetry makes local logs, `corral doctor`, deterministic
  fixtures, and opt-in contract tests the primary support evidence.
