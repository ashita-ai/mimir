# ADR-0003: Job Queue — riverqueue/river

**Status:** Accepted
**Date:** 2026-03-18

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
