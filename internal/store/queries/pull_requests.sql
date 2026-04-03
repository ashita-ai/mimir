-- name: UpsertPullRequest :one
INSERT INTO pull_requests (
    id, github_pr_id, repo_full_name, pr_number,
    head_sha, base_sha, author, state, metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (github_pr_id, head_sha)
DO UPDATE SET
    state      = EXCLUDED.state,
    metadata   = EXCLUDED.metadata,
    updated_at = now()
RETURNING id, created_at, updated_at;

-- name: GetPullRequest :one
SELECT id, github_pr_id, repo_full_name, pr_number,
       head_sha, base_sha, author, state, metadata,
       deleted_at, created_at, updated_at
FROM pull_requests
WHERE id = $1;

-- name: SoftDeletePullRequest :execrows
UPDATE pull_requests
SET deleted_at = now()
WHERE id = $1 AND deleted_at IS NULL;
