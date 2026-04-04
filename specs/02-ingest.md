# Spec 02: Ingest — GitHub Integration

> **Status:** Draft
> **Date:** 2026-03-27
> **Package:** `internal/ingest`
> **Implements:** `pkg/adapter.ProviderAdapter`

---

## Responsibilities

1. GitHub App authentication (JWT + installation token exchange)
2. Webhook reception and signature verification
3. PR data fetching (metadata, diff, commits)
4. Comment posting (inline + summary)
5. Reaction polling for eval feedback loop
6. `addressed_in_next_commit` content-hash re-check on new pushes

---

## GitHub App Authentication

### Token Exchange Flow

```
1. Load private key from MIMIR_GITHUB_APP_PRIVATE_KEY_PATH
2. Build JWT:
   - iss: MIMIR_GITHUB_APP_ID
   - iat: now - 60s (clock skew tolerance)
   - exp: now + 10min (GitHub max)
   - alg: RS256
3. POST /app/installations/{installation_id}/access_tokens
   - Authorization: Bearer {jwt}
   - Body: { "permissions": { "pull_requests": "write", "contents": "read" } }
4. Response: { "token": "ghs_...", "expires_at": "2026-03-27T..." }
5. Cache token in memory. Refresh when < 5 min until expiry.
```

### Installation ID Resolution

The webhook payload includes `installation.id`. For `mimir review` CLI mode, the installation ID must be resolved:

```
GET /repos/{owner}/{repo}/installation
Authorization: Bearer {jwt}
→ { "id": 12345, ... }
```

Cache the `(repo_full_name → installation_id)` mapping in memory.

### Installation Changes

When a repository changes its GitHub App installation (e.g., the app is uninstalled and reinstalled under a different org), the cached `installation_id` becomes stale. Handle this by:

1. If a 404 or 403 is returned from the installation token exchange, evict the cached mapping.
2. Re-resolve the installation ID via `GET /repos/{owner}/{repo}/installation`.
3. If re-resolution fails, mark the pipeline run as failed with a clear error: `"installation not found: app may have been uninstalled from this repository"`.
4. Log the stale-to-new mapping transition for operational visibility.

### PAT Fallback

When `MIMIR_GITHUB_TOKEN` is set and GitHub App credentials are absent, use the PAT directly. No JWT exchange, no installation scoping. This is the CLI-mode path.

Detection logic:
```go
if cfg.GitHubAppID != 0 && cfg.GitHubAppPrivateKeyPath != "" {
    // GitHub App mode
} else if cfg.GitHubToken != "" {
    // PAT mode
} else {
    // Fatal: no GitHub credentials configured
}
```

---

## Webhook Handler

### Endpoint

`POST /v1/webhooks/github`

### Event Filter

Accept only `pull_request` events with actions: `opened`, `synchronize`, `reopened`.

Reject all other events with 200 OK (GitHub retries on non-2xx, so always return 200 even for ignored events).

### Signature Verification

```go
func verifySignature(secret []byte, payload []byte, signatureHeader string) error {
    // signatureHeader = "sha256=abc123..."
    if !strings.HasPrefix(signatureHeader, "sha256=") {
        return errors.New("unsupported signature algorithm")
    }
    expected := hmac.New(sha256.New, secret)
    expected.Write(payload)
    expectedMAC := hex.EncodeToString(expected.Sum(nil))
    actual := signatureHeader[len("sha256="):]
    if !hmac.Equal([]byte(expectedMAC), []byte(actual)) {
        return errors.New("signature mismatch")
    }
    return nil
}
```

This runs as chi middleware, before the handler parses the body. Failed verification → 401, log the attempt, do not enqueue.

### Job Enqueue

Within a single database transaction:

1. Upsert `pull_requests` row (keyed on `external_pr_id + head_sha`)
2. Insert River `ReviewPipelineJob` with the PR ID as payload
3. Commit

If the transaction fails, the webhook returns 500 and GitHub will retry.

```go
type ReviewPipelineJob struct {
    PullRequestID uuid.UUID `json:"pull_request_id"`
}
```

---

## FetchPR

Called inside the River job handler, not in the webhook handler. The webhook only enqueues; the worker fetches.

### API Calls

```
1. GET /repos/{owner}/{repo}/pulls/{number}
   → PR metadata (title, author, state, head/base SHAs)
   Also fetch with Accept: application/vnd.github.v3.diff on the same endpoint
   → Unified diff as plain text
   (These are the same endpoint with different Accept headers; two HTTP calls, one logical resource.)

2. GET /repos/{owner}/{repo}/pulls/{number}/files?per_page=100
   → File list with status (added/modified/removed), paginate if > 100 files
```

### Large Diff Handling

GitHub truncates diffs at 3,000 files or 500KB. If the diff response indicates truncation:

1. Log a warning with the PR URL
2. Proceed with the truncated diff
3. Set a flag on `core.PullRequest` so the summary comment can disclose: "This PR exceeded GitHub's diff size limit. Review coverage is partial."

The pipeline does not attempt to reconstruct the full diff via `git clone`. That's M2 scope.

### Output

Returns `*core.PullRequest` with all fields populated. `Metadata` JSONB stores the full GitHub API response for fields we don't model explicitly (labels, milestone, draft status, etc.).

---

## PostComment (Inline)

Posts a finding as a review comment on a specific diff line.

### GitHub API

```
POST /repos/{owner}/{repo}/pulls/{number}/comments
{
  "body": "...",
  "commit_id": "{head_sha}",
  "path": "{file_path}",
  "line": {line_number},
  "side": "RIGHT"
}
```

### Line Number Mapping

GitHub review comments use the line number in the **new version** of the file (the `RIGHT` side of the diff). Mimir's `Finding.StartLine` is already in new-file coordinates (the index extracts line numbers from the head SHA, not the base).

If `StartLine` is nil (file-level finding), post as a regular PR comment instead of an inline review comment.

### Comment Body

```markdown
**{severity}** | {category} | {confidence_tier} confidence

### {title}

{body}

{suggestion block, if present}

---
Was this helpful? React with :+1: or :-1:
```

Suggestion block (when `Finding.Suggestion` is non-empty):

````markdown
```suggestion
{suggestion}
```
````

GitHub renders this as a "suggested change" that the author can apply with one click.

### Idempotency

Before posting, query `findings` for this `location_hash + pull_request_id`. If `external_comment_id` is already set, skip. This prevents duplicate comments on River job retries.

### Return Value

Returns the GitHub comment ID (`int64`). The caller calls `store.MarkFindingPosted(findingID, commentID)`.

---

## PostSummaryComment

Posts a single top-level PR comment with the full review summary.

### GitHub API

```
POST /repos/{owner}/{repo}/issues/{number}/comments
{
  "body": "..."
}
```

(PR comments use the issues endpoint.)

### Summary Template

See `specs/12-prompts.md` for the full template. Key sections:

1. Coverage line: "X/Y functions reviewed (Z tasks failed)"
2. Inline findings table (findings posted as review comments)
3. Additional findings table (medium-confidence, summary-only)
4. Incomplete section (failed tasks with error descriptions)
5. Approximate context disclosure (when `IsApproximate()` is true)
6. Footer with reaction instructions

### Partial Completion Notification

When the pipeline pauses or halts mid-review (circuit breaker tripped, rate limit exhaustion, context cancellation), post the summary comment immediately with the findings collected so far. The summary comment must include a disclosure banner:

```markdown
> **Warning: Partial review:** Mimir stopped after reviewing {completed}/{total} tasks.
> Reason: {reason}. Remaining tasks were not reviewed.
```

This ensures users are never left wondering whether the review is complete. The banner appears above the findings table in the summary comment. Even if zero findings were collected, the summary comment is posted so the user sees the partial-review notice.

### Idempotency

Store the summary comment ID in `pull_requests.metadata` JSONB under key `summary_comment_id`. On retry, if the key exists, edit the existing comment instead of posting a new one:

```
PATCH /repos/{owner}/{repo}/issues/comments/{comment_id}
```

---

## Reaction Polling

Collects eval signal from GitHub reactions on Mimir's inline comments.

### Implementation

A periodic River job (`ReactionPollJob`) runs every 15 minutes:

1. Query `findings` where `external_comment_id IS NOT NULL` and `posted_at > now() - interval '7 days'`
2. For each finding, check reactions:
   ```
   GET /repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions
   ```
3. Write new reactions to `finding_events`:
   ```sql
   INSERT INTO finding_events (finding_id, event_type, actor, created_at)
   VALUES ($1, 'thumbs_up', $2, $3)
   ON CONFLICT DO NOTHING;
   ```

### Rate Limit Awareness

This job makes one API call per recently-posted finding. For a team generating ~50 findings/day, that's ~50 calls per 15-minute cycle — well within GitHub's rate limits. If `X-RateLimit-Remaining` drops below 100, pause and resume on the next cycle.

---

## Addressed-in-Next-Commit Detection (M2)

**Deferred to M2.** The natural dedup mechanism handles the most common case: when a developer fixes a flagged function and pushes again, the content hash changes. The planner's task-level dedup (Spec 04) sees the content hash mismatch, generates a new task, and the model re-reviews the updated code. If the fix resolves the issue, the model simply won't produce the same finding — the old finding stays in the DB as unaddressed, but no new duplicate is posted.

The M2 implementation will add explicit detection:

1. On `synchronize` events, re-parse flagged files at the new head SHA
2. Compare content hashes to detect code changes in flagged regions
3. Transition findings to `likely_addressed` when the underlying code changed
4. Surface addressed status in the summary comment

The `addressed_status` column and CHECK constraint are already in the schema (Spec 07) to support this without migration changes when M2 lands.
