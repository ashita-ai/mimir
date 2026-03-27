# Spec 02: Ingest — GitHub Integration

> **Status:** Reviewed
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

Cache the `(repo_full_name → installation_id)` mapping in memory. Repos don't change installations often.

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

`POST /webhooks/github`

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

1. Upsert `pull_requests` row (keyed on `github_pr_id + head_sha`)
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

2. GET /repos/{owner}/{repo}/pulls/{number}
   Accept: application/vnd.github.v3.diff
   → Unified diff as plain text

3. GET /repos/{owner}/{repo}/pulls/{number}/files?per_page=100
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

Before posting, query `findings` for this `location_hash + pull_request_id`. If `github_comment_id` is already set, skip. This prevents duplicate comments on River job retries.

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

1. Query `findings` where `github_comment_id IS NOT NULL` and `posted_at > now() - interval '7 days'`
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

## Addressed-in-Next-Commit Detection

Triggered when a `synchronize` event arrives (new push to an open PR).

### Algorithm

```
1. Fetch findings from the previous head SHA for this PR:
   SELECT * FROM findings
   WHERE pull_request_id = $1
     AND addressed_status = 'unaddressed'
     AND content_hash IS NOT NULL

2. For each previous finding:
   a. Check if the file still exists at the new head SHA
   b. If file exists: re-run tree-sitter parse on the file,
      extract the AST subtree for the same symbol
   c. Compute new content_hash = sha256(new AST subtree)
   d. If old content_hash != new content_hash:
      UPDATE findings SET addressed_status = 'likely_addressed'
      WHERE id = $1
   e. If symbol no longer exists in the file:
      UPDATE findings SET addressed_status = 'likely_addressed'
      WHERE id = $1

3. Findings where content_hash IS NULL are skipped (no AST was available).
   Findings where the file is unchanged are left as 'unaddressed'.
```

This runs as part of the pipeline job for the new push, before generating new tasks. Cost: one tree-sitter parse per previously-flagged file. No LLM calls.

### Schema Note

The `addressed_status` column is `TEXT NOT NULL DEFAULT 'unaddressed'` with a CHECK constraint on `('unaddressed', 'likely_addressed', 'confirmed')`. Option C (re-inference confirmation) can be added later by implementing the `'confirmed'` transition without schema changes.
