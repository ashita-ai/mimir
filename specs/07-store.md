# Spec 07: Store — Database Schema & Persistence

> **Status:** M1 Build Spec
> **Date:** 2026-03-27
> **Package:** `internal/store`
> **Implements:** `pkg/adapter.StoreAdapter`
> **ADR:** [0002-database-postgresql.md](../adr/0002-database-postgresql.md)

---

## Responsibilities

1. Own all database schema (goose migrations)
2. Implement `StoreAdapter` via sqlc-generated queries
3. Manage connection pooling (`pgx/v5`)
4. Provide transaction support where needed

---

## Schema

### Migration 001: Core Tables

```sql
-- 001_create_core_tables.sql

CREATE TABLE pull_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_pr_id    BIGINT NOT NULL,
    repo_full_name  TEXT NOT NULL,
    pr_number       INT NOT NULL,
    head_sha        TEXT NOT NULL,
    base_sha        TEXT NOT NULL,
    author          TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('open', 'closed', 'merged')),
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (github_pr_id, head_sha)
);

CREATE INDEX pull_requests_repo_pr_idx ON pull_requests (repo_full_name, pr_number);

CREATE TABLE review_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    task_type       TEXT NOT NULL CHECK (task_type IN ('security', 'logic', 'test_coverage', 'style')),
    file_path       TEXT NOT NULL,
    symbol          TEXT NOT NULL DEFAULT '',
    risk_score      FLOAT NOT NULL,
    model_id        TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    error           TEXT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX review_tasks_pr_id_idx ON review_tasks (pull_request_id);
CREATE INDEX review_tasks_status_idx ON review_tasks (status) WHERE status = 'pending';

CREATE TABLE findings (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_task_id      UUID NOT NULL REFERENCES review_tasks(id) ON DELETE CASCADE,
    pull_request_id     UUID NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,

    -- Location
    file_path           TEXT NOT NULL,
    start_line          INT,
    end_line            INT,
    symbol              TEXT NOT NULL DEFAULT '',

    -- Classification
    category            TEXT NOT NULL
                        CHECK (category IN ('security', 'logic', 'test_coverage', 'style', 'performance')),
    confidence_tier     TEXT NOT NULL CHECK (confidence_tier IN ('high', 'medium', 'low')),
    confidence_score    FLOAT NOT NULL CHECK (confidence_score BETWEEN 0 AND 1),
    severity            TEXT NOT NULL
                        CHECK (severity IN ('critical', 'high', 'medium', 'low', 'info')),

    -- Content
    title               TEXT NOT NULL,
    body                TEXT NOT NULL,
    suggestion          TEXT,

    -- Fingerprinting
    location_hash       TEXT NOT NULL,
    content_hash        TEXT,

    -- Lifecycle
    posted_at           TIMESTAMPTZ,
    github_comment_id   BIGINT,
    addressed_status    TEXT NOT NULL DEFAULT 'unaddressed'
                        CHECK (addressed_status IN ('unaddressed', 'likely_addressed', 'confirmed')),
    dismissed_at        TIMESTAMPTZ,
    dismissed_by        TEXT,

    -- Provenance
    model_id            TEXT NOT NULL,
    prompt_tokens       INT,
    completion_tokens   INT,
    metadata            JSONB NOT NULL DEFAULT '{}',

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX findings_pr_id_idx ON findings (pull_request_id);
CREATE INDEX findings_location_hash_idx ON findings (location_hash);
CREATE UNIQUE INDEX findings_location_hash_pr_idx ON findings (location_hash, pull_request_id);
CREATE INDEX findings_addressed_status_idx ON findings (addressed_status)
    WHERE addressed_status = 'unaddressed';

CREATE TABLE dismissed_fingerprints (
    fingerprint     TEXT NOT NULL,
    repo_full_name  TEXT NOT NULL,
    dismissed_by    TEXT NOT NULL,
    reason          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fingerprint, repo_full_name)
);

CREATE TABLE finding_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    finding_id  UUID NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    event_type  TEXT NOT NULL CHECK (event_type IN ('thumbs_up', 'thumbs_down', 'dismissed', 'resolved')),
    actor       TEXT NOT NULL,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (finding_id, event_type, actor)
);

CREATE INDEX finding_events_finding_id_idx ON finding_events (finding_id);
```

### River Tables

River manages its own tables (`river_job`, `river_leader`, `river_queue`, etc.) via its built-in migrator. Do not create or modify these manually. River's migration runs at startup:

```go
migrator, _ := rivermigrate.New(riverpgxv5.New(pool), nil)
migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
```

---

## sqlc Configuration

```yaml
# sqlc.yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "internal/store/queries/"
    schema: "internal/store/migrations/"
    gen:
      go:
        package: "store"
        out: "internal/store/generated"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_pointers_for_null_types: true
```

### Query Files

Organized by domain entity:

```
internal/store/queries/
├── pull_requests.sql
├── review_tasks.sql
├── findings.sql
├── dismissed_fingerprints.sql
└── finding_events.sql
```

### Key Queries

**pull_requests.sql:**
```sql
-- name: UpsertPullRequest :one
INSERT INTO pull_requests (github_pr_id, repo_full_name, pr_number, head_sha, base_sha, author, state, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (github_pr_id, head_sha) DO UPDATE
SET state = EXCLUDED.state, metadata = EXCLUDED.metadata, updated_at = now()
RETURNING *;

-- name: GetPullRequest :one
SELECT * FROM pull_requests WHERE id = $1;

-- name: GetPullRequestByGitHubID :one
SELECT * FROM pull_requests
WHERE github_pr_id = $1
ORDER BY created_at DESC
LIMIT 1;
```

**review_tasks.sql:**
```sql
-- name: CreateReviewTask :one
INSERT INTO review_tasks (pull_request_id, task_type, file_path, symbol, risk_score, model_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateReviewTaskStatus :exec
UPDATE review_tasks
SET status = $2, error = $3, started_at = CASE WHEN $2 = 'running' THEN now() ELSE started_at END,
    completed_at = CASE WHEN $2 IN ('completed', 'failed') THEN now() ELSE completed_at END
WHERE id = $1;

-- name: ListReviewTasksForPR :many
SELECT * FROM review_tasks WHERE pull_request_id = $1;

-- name: CountCompletedAndTotal :one
SELECT
    COUNT(*) AS total,
    COUNT(*) FILTER (WHERE status = 'completed') AS completed,
    COUNT(*) FILTER (WHERE status = 'failed') AS failed
FROM review_tasks
WHERE pull_request_id = $1;
```

**findings.sql:**
```sql
-- name: CreateFinding :one
INSERT INTO findings (
    review_task_id, pull_request_id, file_path, start_line, end_line, symbol,
    category, confidence_tier, confidence_score, severity,
    title, body, suggestion, location_hash, content_hash,
    model_id, prompt_tokens, completion_tokens, metadata
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (location_hash, pull_request_id) DO UPDATE
SET confidence_score = EXCLUDED.confidence_score,
    confidence_tier = EXCLUDED.confidence_tier,
    body = EXCLUDED.body,
    content_hash = EXCLUDED.content_hash,
    updated_at = now()
RETURNING *;

-- name: ListFindingsForPR :many
SELECT * FROM findings WHERE pull_request_id = $1 ORDER BY severity, confidence_score DESC;

-- name: MarkFindingPosted :exec
UPDATE findings SET posted_at = now(), github_comment_id = $2 WHERE id = $1;

-- name: MarkFindingAddressed :exec
UPDATE findings SET addressed_status = $2, updated_at = now() WHERE id = $1;

-- name: FindPriorFinding :one
SELECT location_hash, content_hash FROM findings
WHERE pull_request_id = $1 AND location_hash = $2 AND addressed_status = 'unaddressed'
ORDER BY created_at DESC
LIMIT 1;

-- name: ListUnaddressedFindingsForPR :many
SELECT * FROM findings
WHERE pull_request_id = $1
  AND addressed_status = 'unaddressed'
  AND content_hash IS NOT NULL;
```

**dismissed_fingerprints.sql:**
```sql
-- name: IsFingerprintDismissed :one
SELECT EXISTS(
    SELECT 1 FROM dismissed_fingerprints
    WHERE fingerprint = $1 AND repo_full_name = $2
) AS dismissed;

-- name: DismissFingerprint :exec
INSERT INTO dismissed_fingerprints (fingerprint, repo_full_name, dismissed_by, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;
```

**finding_events.sql:**
```sql
-- name: CreateFindingEvent :exec
INSERT INTO finding_events (finding_id, event_type, actor, metadata)
VALUES ($1, $2, $3, $4)
ON CONFLICT (finding_id, event_type, actor) DO NOTHING;

-- name: CountReactionsForFinding :one
SELECT
    COUNT(*) FILTER (WHERE event_type = 'thumbs_up') AS thumbs_up,
    COUNT(*) FILTER (WHERE event_type = 'thumbs_down') AS thumbs_down
FROM finding_events
WHERE finding_id = $1;
```

---

## StoreAdapter Implementation

```go
type PostgresStore struct {
    pool    *pgxpool.Pool
    queries *generated.Queries
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
    return &PostgresStore{
        pool:    pool,
        queries: generated.New(pool),
    }
}
```

### Transaction Support

Operations that must be atomic (e.g., webhook receive: upsert PR + enqueue River job):

```go
func (s *PostgresStore) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
    tx, err := s.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback(ctx)
    if err := fn(tx); err != nil {
        return err
    }
    return tx.Commit(ctx)
}
```

River's `InsertTx` method accepts the transaction, ensuring job enqueue is atomic with PR persistence.

---

## Connection Pool Configuration

```go
poolConfig, _ := pgxpool.ParseConfig(cfg.DatabaseURL)
poolConfig.MaxConns = 20                          // default; tune per instance
poolConfig.MinConns = 2
poolConfig.HealthCheckPeriod = 30 * time.Second
poolConfig.MaxConnLifetime = 30 * time.Minute
poolConfig.MaxConnIdleTime = 5 * time.Minute
```

These defaults are for a single `mimir serve` instance. In multi-instance deployments, `MaxConns` should be lowered to avoid exhausting PostgreSQL's `max_connections`. Document this in the config spec.
