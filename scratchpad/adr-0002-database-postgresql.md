# ADR-0002: Database — PostgreSQL Only

> **Status:** Under discussion — reopened from Accepted for design review.
> Original decision date: 2026-03-18.

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

---

## Review Notes (2026-03-21)

### Dual-Hash Fingerprinting

The finding schema uses fingerprinting to deduplicate findings across review runs. The original design proposed a single hash: `sha256(symbol + file_path + category + normalized_body)`. This is both too brittle (breaks on file moves) and too unstable (LLM output is nondeterministic, so `normalized_body` changes across runs even for the same finding).

**Proposed: two-hash approach.**

| Hash | Formula | Purpose |
|------|---------|---------|
| **Location hash** | `sha256(repo + file_path + symbol + category)` | Dedup across runs: "have we flagged this spot for this issue type?" |
| **Content hash** | `sha256(AST subtree of the flagged code region)` | Detect whether the code actually changed since last flagged |

**Dedup rule:** Suppress a finding if its location hash matches a recent finding AND the content hash is unchanged. Re-surface if the location hash matches but the content hash differs — the code changed, but the same problem may persist.

This keeps all fingerprinting deterministic (no LLM output in the hash), survives line number changes within a file, and correctly re-surfaces findings when code is modified. The `content_hash` column already exists in the finding schema draft — this change connects it to the dedup logic.

**Schema impact:** Replace the single `fingerprint` column with `location_hash` and keep `content_hash`. The `dismissed_fingerprints` table keys on `location_hash`. The unique index becomes `(location_hash, pull_request_id)`.

### No Graph DB, No Vector DB

PostgreSQL handles everything Mimir persists. The semantic index (ADR-0004) is ephemeral per-run data held in memory. There is no query pattern that requires graph traversal or vector similarity search in M1 or M2. If semantic dedup via embeddings is explored in M3+, `pgvector` (a PostgreSQL extension) would be evaluated before any standalone vector DB.
