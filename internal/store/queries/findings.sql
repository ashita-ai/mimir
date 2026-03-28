-- name: CreateFinding :one
INSERT INTO findings (
    id, review_task_id, pull_request_id,
    file_path, start_line, end_line, symbol,
    category, confidence_tier, confidence_score, severity,
    title, body, suggestion,
    location_hash, content_hash,
    model_id, prompt_tokens, completion_tokens, metadata
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16,
    $17, $18, $19, $20
)
RETURNING created_at, updated_at;

-- name: ListFindingsForPR :many
SELECT id, review_task_id, pull_request_id,
       file_path, start_line, end_line, symbol,
       category, confidence_tier, confidence_score, severity,
       title, body, suggestion,
       location_hash, content_hash,
       posted_at, github_comment_id, addressed_in_next_commit,
       dismissed_at, dismissed_by,
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
