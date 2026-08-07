# ADR-0001: Implementation language — Go

Status: accepted · Date: 2026-08-07

## Context

Corral is a long-running daemon that supervises `claude` CLI processes: PTY management, unix-socket API, JSON parsing (stream-json, hook payloads), SQLite persistence, task orchestration. Candidates considered: C, Rust, Go.

## Decision

Go, with `CGO_ENABLED=0` (pure-Go SQLite via `modernc.org/sqlite`) to keep single-static-binary distribution and trivial cross-compilation.

## Rationale

- The workload is I/O-bound process supervision; the LLM is the latency floor. C/Rust performance buys nothing measurable.
- The competitive variable is iteration speed against a weekly-moving target (Claude Code) and an established competitor (Herdr). Go's compile speed, small language surface, and goroutine-per-agent concurrency model maximize it.
- C rejected: memory/string/JSON handling risk and 3–5× slower development for zero benefit at this layer.
- Rust rejected: superior type system (state modeling) acknowledged, but async Rust learning curve and slower iteration outweigh it here.

## Revisit trigger

If a component ever needs zero-GC determinism or heavy in-process compute, isolate that component — do not rewrite the product.
