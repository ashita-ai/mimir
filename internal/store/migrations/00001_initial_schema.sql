-- +goose Up

-- pull_requests: one row per GitHub PR event received.
-- Unique on (github_pr_id, head_sha) so a force-push creates a new row.
-- Soft-deleted via deleted_at (spec 07: ON DELETE RESTRICT everywhere, no cascades).
CREATE TABLE pull_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_pr_id    BIGINT      NOT NULL,
    repo_full_name  TEXT        NOT NULL,
    pr_number       INT         NOT NULL,
    head_sha        TEXT        NOT NULL,
    base_sha        TEXT        NOT NULL,
    author          TEXT        NOT NULL,
    state           TEXT        NOT NULL CHECK (state IN ('open', 'closed', 'merged')),
    metadata        JSONB       NOT NULL DEFAULT '{}',
    deleted_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (github_pr_id, head_sha)
);

CREATE INDEX pull_requests_repo_pr_idx ON pull_requests (repo_full_name, pr_number);
CREATE INDEX pull_requests_active_idx  ON pull_requests (repo_full_name, pr_number) WHERE deleted_at IS NULL;

-- pipeline_runs: primary audit anchor. One row per pipeline execution.
-- Links to the PR, captures config/prompt version at execution time,
-- and records task/finding counts for reconciliation.
CREATE TABLE pipeline_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID        NOT NULL REFERENCES pull_requests(id),
    head_sha        TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'running'
                                CHECK (status IN ('running', 'completed', 'failed')),
    prompt_version  TEXT        NOT NULL,
    config_hash     TEXT        NOT NULL,
    task_count      INT         NOT NULL DEFAULT 0,
    finding_count   INT         NOT NULL DEFAULT 0,
    error           TEXT,
    metadata        JSONB       NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX pipeline_runs_pull_request_id_idx ON pipeline_runs (pull_request_id);
CREATE UNIQUE INDEX pipeline_runs_active_idx ON pipeline_runs (pull_request_id, head_sha)
    WHERE status = 'running';

-- review_tasks: one row per logical review unit (function/type examined through one lens).
CREATE TABLE review_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pull_request_id UUID        NOT NULL REFERENCES pull_requests(id),
    pipeline_run_id UUID        NOT NULL REFERENCES pipeline_runs(id),
    task_type       TEXT        NOT NULL CHECK (task_type IN ('security', 'logic', 'test_coverage', 'style')),
    file_path       TEXT        NOT NULL,
    symbol          TEXT,
    risk_score      FLOAT       NOT NULL,
    model_id        TEXT        NOT NULL,
    diff_hunk       TEXT,
    status          TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    error           TEXT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX review_tasks_pull_request_id_idx  ON review_tasks (pull_request_id);
CREATE INDEX review_tasks_pipeline_run_id_idx  ON review_tasks (pipeline_run_id);
CREATE INDEX review_tasks_status_idx           ON review_tasks (status) WHERE status = 'pending';

-- findings: review observations produced by models or static tools.
CREATE TABLE findings (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    review_task_id            UUID        NOT NULL REFERENCES review_tasks(id),
    pull_request_id           UUID        NOT NULL REFERENCES pull_requests(id),
    pipeline_run_id           UUID        NOT NULL REFERENCES pipeline_runs(id),

    -- Location
    file_path                 TEXT        NOT NULL,
    start_line                INT,
    end_line                  INT,
    symbol                    TEXT,

    -- Classification
    category                  TEXT        NOT NULL
                              CHECK (category IN ('security', 'logic', 'test_coverage', 'style', 'performance')),
    confidence_tier           TEXT        NOT NULL
                              CHECK (confidence_tier IN ('high', 'medium', 'low')),
    confidence_score          FLOAT       NOT NULL
                              CHECK (confidence_score BETWEEN 0 AND 1),
    severity                  TEXT        NOT NULL
                              CHECK (severity IN ('critical', 'high', 'medium', 'low', 'info')),

    -- Content
    title                     TEXT        NOT NULL,
    body                      TEXT        NOT NULL,
    suggestion                TEXT,

    -- Fingerprinting (ADR-0002: dual-hash dedup)
    location_hash             TEXT        NOT NULL,
    content_hash              TEXT,
    head_sha                  TEXT        NOT NULL,

    -- Lifecycle
    posted_at                 TIMESTAMPTZ,
    github_comment_id         BIGINT,
    addressed_in_next_commit  BOOLEAN     NOT NULL DEFAULT FALSE,
    suppression_reason        TEXT,
    dismissed_at              TIMESTAMPTZ,
    dismissed_by              TEXT,

    -- Provenance
    model_id                  TEXT        NOT NULL,
    prompt_tokens             INT,
    completion_tokens         INT,
    metadata                  JSONB       NOT NULL DEFAULT '{}',

    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX        findings_pull_request_id_idx  ON findings (pull_request_id);
CREATE INDEX        findings_pipeline_run_id_idx  ON findings (pipeline_run_id);
CREATE INDEX        findings_location_hash_idx    ON findings (location_hash);
CREATE UNIQUE INDEX findings_location_hash_pr_idx ON findings (location_hash, pull_request_id, head_sha);
CREATE INDEX        findings_addressed_idx        ON findings (pull_request_id) WHERE addressed_in_next_commit = FALSE AND posted_at IS NOT NULL;
CREATE INDEX        findings_repo_idx             ON findings (pull_request_id, location_hash);

-- dismissed_fingerprints: permanently suppressed location hashes.
-- When a reviewer dismisses a finding, the location_hash goes here so
-- the same finding is never re-posted for this repo.
CREATE TABLE dismissed_fingerprints (
    fingerprint     TEXT        NOT NULL,
    repo_full_name  TEXT        NOT NULL,
    dismissed_by    TEXT        NOT NULL,
    reason          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (fingerprint, repo_full_name)
);

CREATE INDEX dismissed_fingerprints_repo_idx ON dismissed_fingerprints (repo_full_name);

-- finding_events: append-only lifecycle log for findings.
-- Records reactions, status changes, dismissals — the raw signal
-- for eval (spec 08) and the audit trail for every finding lifecycle transition.
CREATE TABLE finding_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    finding_id  UUID        NOT NULL REFERENCES findings(id),
    event_type  TEXT        NOT NULL,
    actor       TEXT        NOT NULL,
    old_value   TEXT,
    new_value   TEXT,
    metadata    JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX finding_events_finding_id_idx ON finding_events (finding_id);
CREATE INDEX finding_events_type_idx       ON finding_events (event_type);
-- Partial unique index: at most one reaction event per actor per finding.
CREATE UNIQUE INDEX finding_events_reaction_idx ON finding_events (finding_id, actor)
    WHERE event_type = 'reaction';

-- updated_at trigger: auto-maintain updated_at on tables that have it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER pull_requests_updated_at
    BEFORE UPDATE ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER pipeline_runs_updated_at
    BEFORE UPDATE ON pipeline_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER findings_updated_at
    BEFORE UPDATE ON findings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- +goose Down

DROP TRIGGER IF EXISTS findings_updated_at ON findings;
DROP TRIGGER IF EXISTS pipeline_runs_updated_at ON pipeline_runs;
DROP TRIGGER IF EXISTS pull_requests_updated_at ON pull_requests;
DROP FUNCTION IF EXISTS set_updated_at;

DROP TABLE IF EXISTS finding_events;
DROP TABLE IF EXISTS dismissed_fingerprints;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS review_tasks;
DROP TABLE IF EXISTS pipeline_runs;
DROP TABLE IF EXISTS pull_requests;
