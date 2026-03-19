# ADR-0005: Service Architecture — Single Binary, Dual Runtime Mode

**Status:** Accepted
**Date:** 2026-03-18

---

## Context

Mimir in service mode requires two runtime roles:

1. **Webhook receiver** — an HTTP server that accepts GitHub events and enqueues review jobs
2. **Pipeline workers** — background goroutines that dequeue jobs, run the review pipeline, and post results

These could be separate binaries/processes, or they could be co-located in a single binary.

---

## Decision

**Single binary with two runtime modes, both started by `mimir serve`.**

`mimir serve` starts:
- A `chi` HTTP server on a configurable port (webhook receiver, health check, metrics endpoint)
- One or more `river` worker pools (pipeline workers) in the same process

Both are started together by default. Worker concurrency is configurable via flags/env.

The `mimir review` subcommand provides a standalone CLI path for one-shot PR review without a running server (useful for local development and CI integration).

---

## Consequences

**Positive:**
- One binary to build, one Docker image to ship, one process to monitor in simple deployments
- Webhook receiver is stateless (just enqueues jobs) — horizontal scaling is `docker run` more instances against the same DB
- Workers compete via `SKIP LOCKED` — no coordination required between instances
- Local dev is `mimir serve` — no separate worker process to start

**Scaling path (no architectural changes required):**
- To scale workers independently: run additional `mimir serve --http=false` instances (workers only, no HTTP)
- To scale HTTP independently: run `mimir serve --workers=0` (HTTP only, no workers) behind a load balancer
- Both modes share the same binary and config — no new artifact required

**Negative / Accepted trade-offs:**
- In-process failure of the HTTP server takes down workers and vice versa. Mitigated by: river's at-least-once delivery (in-flight jobs restart on next worker), and the fact that `chi` HTTP handling is extremely stable
- Not appropriate for very high webhook volume where HTTP and worker pools would need to scale at different ratios. Out of scope for M1/M2.

**Rejected alternatives:**
- Separate worker binary: More operational complexity (two images, two deployments) with no M1/M2 benefit. Can be introduced later without breaking the `mimir serve` path.
- Message broker (Kafka, RabbitMQ) between HTTP and workers: Adds infrastructure. River on PostgreSQL (ADR-0003) provides equivalent at-least-once semantics without a separate broker.
