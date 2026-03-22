# ADR-0001: Implementation Language — Go

> **Status:** Under discussion — reopened from Accepted for design review.
> Original decision date: 2026-03-18.

---

## Context

Mimir is a webhook-driven service that issues concurrent LLM calls and runs background pipeline workers. We need to choose an implementation language that:

- Handles concurrent I/O-bound work (many LLM HTTP calls per review job) without threading complexity
- Compiles to a single static binary for simple deployment
- Has strong PostgreSQL and HTTP ecosystem support
- Is maintainable by a small team (2–5 engineers)

The team considered Python (primary prior experience) and Go.

All model interactions are REST calls to external provider APIs — there is no local inference. This makes Python's LLM SDK ecosystem advantage irrelevant.

---

## Decision

**Go 1.23.**

- Goroutines + `errgroup` for fan-out within a review job (parallel chunk analysis, parallel tool calls)
- Single static binary (`CGO_ENABLED=0`) simplifies Docker and deployment — no interpreter, no dependency hell
- `pgx/v5` is an excellent PostgreSQL driver; `river` (the job queue) is Go-native and PostgreSQL-backed
- `go-github` provides a mature GitHub API client
- Compile-time type safety reduces a class of runtime bugs critical to catch early in a small team
- `chi` provides lightweight, idiomatic HTTP routing without a heavy framework

---

## Consequences

**Positive:**
- Goroutine-per-chunk fan-out is straightforward and cheap
- Single binary simplifies the Dockerfile and CI artifact
- Strong static typing catches integration errors at compile time

**Negative / Accepted trade-offs:**
- Tree-sitter Go bindings (`smacker/go-tree-sitter`) are thinner than Python's `tree-sitter` bindings. Acceptable given M1 scope (syntax structure + heuristic import analysis only)
- Go generics are newer and less ergonomic than Python for some data transformation patterns. Mitigated by keeping data transformation in SQL (sqlc) where possible

**Superseded alternatives:**
- Python: More LLM tooling, but interpreter overhead, GIL constraints on CPU-bound work, and dependency management complexity outweigh benefits when all model calls are HTTP

---

## Review Notes (2026-03-21)

This decision is sound and unchanged by the design improvements proposed for the index, runtime, and policy layers. Two implementation patterns to lock down early:

1. **Bounded `errgroup` in runtime fan-out.** Use `errgroup` with a semaphore (`SetLimit(n)`) — not unlimited goroutine fan-out. Individual task failures must be isolated (recorded in `review_tasks.status = 'failed'`) without canceling sibling tasks. This means using a custom error-collecting pattern rather than `errgroup`'s default cancel-on-first-error behavior.

2. **`context.Context` propagation is non-negotiable.** Every adapter call, every LLM inference, every tree-sitter parse must accept and respect `ctx`. This is already stated in the plugin interface design principles but bears repeating — it's how we enforce the P95 < 90s latency target via per-task timeouts.
