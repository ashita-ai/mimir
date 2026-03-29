-- name: CreateFindingEvent :one
INSERT INTO finding_events (
    id, finding_id, event_type, actor,
    old_value, new_value, metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at;

-- name: ListEventsForFinding :many
SELECT id, finding_id, event_type, actor,
       old_value, new_value, metadata, created_at
FROM finding_events
WHERE finding_id = $1
ORDER BY created_at ASC;
