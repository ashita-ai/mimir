# ADR-0002: Database — PostgreSQL Only

**Status:** Accepted
**Date:** 2026-03-18

---

## Context

Mimir needs persistent storage for findings, review tasks, events, and job queue state. Requirements:

- ACID job delivery for the review pipeline (at-least-once, with retry/backoff)
- JSONB storage for flexible finding metadata
- Concurrent writes from multiple worker goroutines
- Local dev experience that doesn't require standing up a managed cloud DB

A dual-database design (SQLite for CLI/CI, PostgreSQL for service mode) was considered and rejected.

---

## Decision

**PostgreSQL 16+ only.** Docker Compose provides a zero-config local dev environment.

- `docker compose up -d` is the single command to get a running DB for local dev and CI
- JSONB is available for finding metadata without schema churn as the model changes
- `river` (ADR-0003) is PostgreSQL-backed and requires PostgreSQL — a SQLite path would block it
- No need to maintain two migration dialects or restrict the schema to lowest-common-denominator SQL

CLI mode connects to a local PostgreSQL instance (started via Docker Compose). This is an explicit design choice: Mimir is not a zero-infrastructure tool.

---

## Consequences

**Positive:**
- Full JSONB support — finding metadata, LLM response envelopes, and tool outputs stored without rigid columns
- `river` job queue is unlocked (see ADR-0003)
- Single migration dialect (goose + PostgreSQL) — no portability constraints on schema design
- `pgx/v5` pool is production-grade; connection pooling works correctly in concurrent worker mode

**Negative / Accepted trade-offs:**
- CLI users must have Docker installed. This is an accepted constraint: Mimir is built for engineering teams, not individual developers on locked-down machines
- No embedded/serverless option. Explicitly out of scope for M1

**Superseded alternatives:**
- SQLite + PostgreSQL dual mode: Considered and rejected. Dual migration dialects, no JSONB, no array types, and `river` requires PostgreSQL anyway. The portability gain was not worth the complexity cost.
