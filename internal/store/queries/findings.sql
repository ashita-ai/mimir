-- name: CreateFinding :one
INSERT INTO findings (
    id, review_task_id, pull_request_id, pipeline_run_id,
    file_path, start_line, end_line, symbol,
    category, confidence_tier, confidence_score, severity,
    title, body, suggestion,
    location_hash, content_hash, head_sha,
    model_id, prompt_tokens, completion_tokens, metadata
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17, $18,
    $19, $20, $21, $22
)
RETURNING created_at, updated_at;

-- name: ListFindingsForPR :many
SELECT id, review_task_id, pull_request_id, pipeline_run_id,
       file_path, start_line, end_line, symbol,
       category, confidence_tier, confidence_score, severity,
       title, body, suggestion,
       location_hash, content_hash, head_sha,
       posted_at, github_comment_id, addressed_in_next_commit,
       suppression_reason, dismissed_at, dismissed_by,
       model_id, prompt_tokens, completion_tokens, metadata,
       created_at, updated_at
FROM findings
WHERE pull_request_id = $1
ORDER BY confidence_score DESC,
         CASE severity
             WHEN 'critical' THEN 1
             WHEN 'high'     THEN 2
             WHEN 'medium'   THEN 3
             WHEN 'low'      THEN 4
             WHEN 'info'     THEN 5
         END ASC;

-- name: FindPriorFinding :one
SELECT f.id, f.review_task_id, f.pull_request_id, f.pipeline_run_id,
       f.file_path, f.start_line, f.end_line, f.symbol,
       f.category, f.confidence_tier, f.confidence_score, f.severity,
       f.title, f.body, f.suggestion,
       f.location_hash, f.content_hash, f.head_sha,
       f.posted_at, f.github_comment_id, f.addressed_in_next_commit,
       f.suppression_reason, f.dismissed_at, f.dismissed_by,
       f.model_id, f.prompt_tokens, f.completion_tokens, f.metadata,
       f.created_at, f.updated_at
FROM findings f
JOIN pull_requests pr ON f.pull_request_id = pr.id
WHERE f.location_hash = $1
  AND pr.repo_full_name = $2
ORDER BY f.created_at DESC
LIMIT 1;

-- name: ListUnaddressedFindings :many
SELECT id, review_task_id, pull_request_id, pipeline_run_id,
       file_path, start_line, end_line, symbol,
       category, confidence_tier, confidence_score, severity,
       title, body, suggestion,
       location_hash, content_hash, head_sha,
       posted_at, github_comment_id, addressed_in_next_commit,
       suppression_reason, dismissed_at, dismissed_by,
       model_id, prompt_tokens, completion_tokens, metadata,
       created_at, updated_at
FROM findings
WHERE pull_request_id = $1
  AND posted_at IS NOT NULL
  AND addressed_in_next_commit = FALSE
ORDER BY created_at ASC;

-- name: ListUnpostedFindings :many
SELECT id, review_task_id, pull_request_id, pipeline_run_id,
       file_path, start_line, end_line, symbol,
       category, confidence_tier, confidence_score, severity,
       title, body, suggestion,
       location_hash, content_hash, head_sha,
       posted_at, github_comment_id, addressed_in_next_commit,
       suppression_reason, dismissed_at, dismissed_by,
       model_id, prompt_tokens, completion_tokens, metadata,
       created_at, updated_at
FROM findings
WHERE pull_request_id = $1
  AND posted_at IS NULL
  AND suppression_reason IS NULL
ORDER BY confidence_score DESC;

-- name: MarkFindingPosted :execrows
UPDATE findings
SET posted_at = now(), github_comment_id = $2
WHERE id = $1;

-- name: MarkFindingAddressed :execrows
UPDATE findings
SET addressed_in_next_commit = true
WHERE id = $1;

-- name: IsFingerprintDismissed :one
SELECT EXISTS(
    SELECT 1 FROM dismissed_fingerprints
    WHERE fingerprint = $1 AND repo_full_name = $2
) AS dismissed;
