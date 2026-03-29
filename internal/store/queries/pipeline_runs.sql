-- name: CreatePipelineRun :one
INSERT INTO pipeline_runs (
    id, pull_request_id, head_sha, status,
    prompt_version, config_hash, metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at, updated_at;

-- name: CompletePipelineRun :execrows
UPDATE pipeline_runs
SET status        = $2,
    task_count    = $3,
    finding_count = $4,
    error         = $5,
    completed_at  = now()
WHERE id = $1;

-- name: GetPipelineRun :one
SELECT id, pull_request_id, head_sha, status,
       prompt_version, config_hash,
       task_count, finding_count, error, metadata,
       started_at, completed_at, created_at, updated_at
FROM pipeline_runs
WHERE id = $1;

-- name: ListPipelineRunsForPR :many
SELECT id, pull_request_id, head_sha, status,
       prompt_version, config_hash,
       task_count, finding_count, error, metadata,
       started_at, completed_at, created_at, updated_at
FROM pipeline_runs
WHERE pull_request_id = $1
ORDER BY created_at DESC;

-- name: ReconcileStalePipelineRuns :execrows
UPDATE pipeline_runs
SET status       = 'failed',
    error        = 'reconciled: process crashed or restarted',
    completed_at = now()
WHERE status = 'running';
