# Finding Schema (Draft)

> **Status:** Draft — not yet implemented. See open questions before finalizing.

---

## Core `findings` Table

```sql
CREATE TABLE findings (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_task_id   UUID NOT NULL REFERENCES review_tasks(id),
    pull_request_id  UUID NOT NULL REFERENCES pull_requests(id),

    -- Location
    file_path        TEXT NOT NULL,
    start_line       INT,           -- NULL if finding is file-level
    end_line         INT,           -- NULL if single-line or file-level
    symbol           TEXT,          -- function/type name if applicable

    -- Classification
    category         TEXT NOT NULL, -- 'security' | 'logic' | 'test_coverage' | 'style' | 'performance'
    confidence_tier  TEXT NOT NULL, -- 'high' | 'medium' | 'low'
    confidence_score FLOAT NOT NULL CHECK (confidence_score BETWEEN 0 AND 1),
    severity         TEXT NOT NULL, -- 'critical' | 'high' | 'medium' | 'low' | 'info'

    -- Content
    title            TEXT NOT NULL,
    body             TEXT NOT NULL,
    suggestion       TEXT,          -- Optional: suggested fix (for inline comment)

    -- Fingerprinting (see adr-0002-database-postgresql.md review notes)
    location_hash    TEXT NOT NULL, -- sha256(repo + file_path + symbol + category)
    content_hash     TEXT,          -- sha256(AST subtree of flagged code); NULL if index unavailable

    -- Lifecycle
    posted_at        TIMESTAMPTZ,   -- NULL if not yet posted
    github_comment_id BIGINT,       -- NULL if not yet posted
    addressed_in_next_commit BOOLEAN NOT NULL DEFAULT FALSE,
    dismissed_at     TIMESTAMPTZ,   -- NULL if not dismissed
    dismissed_by     TEXT,          -- GitHub username

    -- Provenance
    model_id         TEXT NOT NULL, -- e.g. 'claude-opus-4-6'
    prompt_tokens    INT,
    completion_tokens INT,
    metadata         JSONB NOT NULL DEFAULT '{}',

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX findings_pull_request_id_idx ON findings (pull_request_id);
CREATE INDEX findings_location_hash_idx ON findings (location_hash);
CREATE UNIQUE INDEX findings_location_hash_pr_idx ON findings (location_hash, pull_request_id);
```

---

## Confidence Tier Model

Confidence is assigned by the model in structured output, then validated and potentially downgraded by the policy layer.

| Tier   | Score Range | Default Posting Policy |
|--------|-------------|------------------------|
| high   | 0.80 – 1.00 | Always post            |
| medium | 0.50 – 0.79 | Post unless suppressed by dedup |
| low    | 0.00 – 0.49 | Suppress unless escalation criteria met |

Tier assignment is **model output**, not derived from `confidence_score` alone. The model is prompted to reason about its confidence and output a tier. The score is a continuous value for sorting and aggregation. Both are stored.

**Escalation overrides:** A `low` confidence `security/critical` finding is escalated to post regardless of tier policy. Escalation is policy-configurable (see ADR-0005 and `scratchpad/plugin-interfaces.md`).

---

## Supporting Tables (Sketch)

```sql
-- One row per GitHub PR event received
CREATE TABLE pull_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_pr_id    BIGINT NOT NULL,
    repo_full_name  TEXT NOT NULL,  -- e.g. 'ashita-ai/mimir'
    pr_number       INT NOT NULL,
    head_sha        TEXT NOT NULL,
    base_sha        TEXT NOT NULL,
    author          TEXT NOT NULL,
    state           TEXT NOT NULL,  -- 'open' | 'closed' | 'merged'
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (github_pr_id, head_sha)
);

-- One row per logical review unit (function/type/migration)
CREATE TABLE review_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID NOT NULL REFERENCES pull_requests(id),
    task_type       TEXT NOT NULL,  -- 'security' | 'logic' | 'test_coverage' | 'style'
    file_path       TEXT NOT NULL,
    symbol          TEXT,
    risk_score      FLOAT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending', -- 'pending' | 'running' | 'completed' | 'failed'
    error           TEXT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Dismissed fingerprints (permanent suppression)
CREATE TABLE dismissed_fingerprints (
    fingerprint     TEXT PRIMARY KEY,
    repo_full_name  TEXT NOT NULL,
    dismissed_by    TEXT NOT NULL,
    reason          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

---

## Open Questions

### [RESOLVED] Fingerprint Durability
**Resolution:** Dual-hash approach. See `adr-0002-database-postgresql.md` review notes.

Replace the single `fingerprint` column with:
- `location_hash TEXT NOT NULL` — `sha256(repo + file_path + symbol + category)` — deterministic, no LLM output
- `content_hash TEXT` — `sha256(AST subtree of flagged code)` — detects whether underlying code changed

Dedup rule: suppress if location hash matches a recent finding AND content hash is unchanged. Re-surface if content hash differs.

**Schema changes:** `fingerprint` → `location_hash`. `dismissed_fingerprints` keys on `location_hash`. Unique index becomes `(location_hash, pull_request_id)`. The existing `content_hash` column is retained and connected to the dedup logic.

File moves without renames: location hash changes (correct — different file is a different location). The finding re-surfaces, which is the right behavior since the reviewer hasn't seen it in this location before.

### [OPEN] `addressed_in_next_commit` Automation
See `design-overview.md` for detection strategy options. The column exists in the schema but the update mechanism is TBD.

Simplest viable: a post-review hook that re-queries findings from the previous head SHA for the same PR, checks if the fingerprint appears in the current run's findings, and flips the flag if not.

### [RESOLVED] Partial Posting
**Resolution:** Two-tier posting. See `adr/0005-service-architecture.md` review notes.

- High-confidence findings: inline comment (max 7 per PR, prioritized by severity)
- Medium-confidence findings: summary comment table only
- Low-confidence findings: suppressed unless escalation criteria met
- Escalated findings (security/critical): always inline, regardless of tier

The schema already supports this — `posted_at` and `github_comment_id` are per-finding for inline comments. The summary comment is a single `PostSummaryComment` call; its ID can be stored in `pull_requests.metadata` JSONB. No new table needed.
