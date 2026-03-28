-- name: CreateReviewTask :one
INSERT INTO review_tasks (
    id, pull_request_id, task_type, file_path, symbol, risk_score, status
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at;

-- name: UpdateReviewTaskStatusRunning :execrows
UPDATE review_tasks
SET status = $2, started_at = now()
WHERE id = $1;

-- name: UpdateReviewTaskStatusTerminal :execrows
UPDATE review_tasks
SET status = $2, error = $3, completed_at = now()
WHERE id = $1;

-- name: UpdateReviewTaskStatusOther :execrows
UPDATE review_tasks
SET status = $2
WHERE id = $1;

-- name: ListPendingReviewTasks :many
SELECT id, pull_request_id, task_type, file_path, symbol,
       risk_score, status, error, started_at, completed_at, created_at
FROM review_tasks
WHERE status = 'pending'
ORDER BY risk_score DESC;
