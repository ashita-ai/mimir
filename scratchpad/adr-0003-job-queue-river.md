# ADR-0003: Job Queue — riverqueue/river

> **Status:** Under discussion — reopened from Accepted for design review.
> Original decision date: 2026-03-18.

---

## Context

Mimir's service mode receives GitHub webhook events and must reliably process them through the review pipeline. Requirements:

- At-least-once delivery with configurable retry/backoff
- ACID — a job enqueued in the same transaction as a DB write either both commit or both roll back
- Visibility into job state (pending, running, completed, failed) for operators
- No additional infrastructure beyond what ADR-0002 already mandates (PostgreSQL)

---

## Decision

**`riverqueue/river`** — PostgreSQL-backed job queue, Go-native, no separate broker.

River uses PostgreSQL advisory locks and `SELECT ... FOR UPDATE SKIP LOCKED` for queue mechanics. Jobs are rows in a PostgreSQL table. Workers poll or are notified via `LISTEN/NOTIFY`. Job insertion is transactional — a webhook handler can insert a job and persist event state atomically.

- No Redis, no RabbitMQ, no Kafka — nothing additional to operate
- Configurable retry with exponential backoff built in
- River UI available for job visibility (M2 scope)
- Go-native API integrates cleanly with `errgroup` fan-out inside job handlers

---

## Consequences

**Positive:**
- Transactional job insertion — webhook reception and job enqueue are a single atomic operation
- Zero additional infrastructure — PostgreSQL is already required (ADR-0002)
- Retry and backoff are configurable per job type
- Horizontal scale: multiple `mimir serve` instances can run against the same DB, competing workers pick up jobs via `SKIP LOCKED`

**Negative / Accepted trade-offs:**
- River is relatively new (2024). Less battle-tested than SQS or RabbitMQ. Mitigated by: PostgreSQL's own reliability, at-least-once semantics, and the fact that review pipeline failures are recoverable (not data-loss scenarios)
- PostgreSQL-only constraint is reinforced. This is a feature, not a bug (see ADR-0002)

**Rejected alternatives:**
- Temporal: Correct tool for human-in-the-loop workflows with multi-day timelines. Review pipeline completes in seconds to minutes — Temporal's operational overhead is not warranted
- Redis + Asynq/BullMQ: Adds an additional stateful dependency. PostgreSQL is already required; adding Redis for queue only increases operational surface
- Database polling (naive): River's `LISTEN/NOTIFY` is preferable to naive polling for latency; `SKIP LOCKED` is standard practice for PostgreSQL queues

---

## Review Notes (2026-03-21)

### Two Levels of Retry

River handles **job-level** retry: if a review pipeline job crashes or times out, River re-enqueues it with exponential backoff. This is correct for infrastructure failures (DB connection lost, OOM, etc.).

But inside a single pipeline run, the runtime fans out N `ReviewTask` executions in parallel. Each task makes an LLM API call that can independently fail (429 rate limit, 500 server error, timeout). These need **task-level** retry with different semantics:

| Concern | Job-level (River) | Task-level (runtime) |
|---------|-------------------|---------------------|
| Scope | Entire pipeline run | Single ReviewTask |
| Trigger | Unhandled panic, job timeout | LLM API error (429, 500, timeout) |
| Retry count | Configurable per job type (default: 3) | 1 retry with exponential backoff |
| Failure behavior | Re-enqueue entire job | Mark task as `failed`, continue siblings |
| 400-class errors | N/A | Zero retries (bad request = bug, not transient) |

**Key principle:** A single failed task must not cancel sibling tasks. If 1 of 10 LLM calls returns a 500, the other 9 findings should still be posted. The failed task is recorded in `review_tasks` with `status = 'failed'` and `error` populated. The summary comment (see ADR-0005 review notes) should note incomplete coverage.

This distinction must be implemented in `internal/runtime`, not in River configuration. River owns job lifecycle; runtime owns task lifecycle.
