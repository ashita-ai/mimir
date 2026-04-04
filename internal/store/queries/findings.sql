-- name: CreateFinding :one
INSERT INTO findings (
    review_task_id, pull_request_id, pipeline_run_id,
    repo_full_name, file_path, start_line, end_line, symbol,
    category, confidence_tier, confidence_score, severity,
    title, body, suggestion, location_hash, content_hash, head_sha,
    suppression_reason,
    model_id, prompt_tokens, completion_tokens, metadata
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
RETURNING *;

-- name: ListFindingsForPR :many
SELECT * FROM findings WHERE pull_request_id = $1 ORDER BY severity, confidence_score DESC;

-- name: ListFindingsForRun :many
SELECT * FROM findings WHERE pipeline_run_id = $1 ORDER BY severity, confidence_score DESC;

-- name: MarkFindingPosted :exec
UPDATE findings SET posted_at = now(), external_comment_id = $2, updated_at = now() WHERE id = $1;

-- name: MarkFindingAddressed :exec
UPDATE findings SET addressed_status = $2, updated_at = now() WHERE id = $1;

-- name: FindPriorFinding :one
SELECT id, location_hash, content_hash, head_sha FROM findings
WHERE pull_request_id = $1 AND location_hash = $2 AND addressed_status = 'unaddressed'
ORDER BY created_at DESC
LIMIT 1;

-- name: ListUnaddressedFindingsForPR :many
SELECT * FROM findings
WHERE pull_request_id = $1
  AND addressed_status = 'unaddressed'
  AND content_hash IS NOT NULL;

-- name: ListUnpostedFindings :many
SELECT f.* FROM findings f
JOIN pipeline_runs pr ON pr.id = f.pipeline_run_id
WHERE f.posted_at IS NULL
  AND f.suppression_reason IS NULL
  AND f.external_comment_id IS NULL
  AND pr.status = 'completed'
  AND f.created_at > now() - interval '7 days'
ORDER BY f.created_at ASC;
