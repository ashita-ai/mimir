# Spec 06: Policy — Filtering, Triage & Posting

> **Status:** Reviewed
> **Date:** 2026-03-27
> **Package:** `internal/policy`
> **Implements:** `pkg/adapter.PolicyAdapter`
> **ADR:** [0005-service-architecture.md](../adr/0005-service-architecture.md) (two-tier posting)

---

## Responsibilities

1. Apply confidence penalties (approximate index)
2. Deduplicate findings against prior runs
3. Check permanent dismissals
4. Triage findings into posting tiers (inline / summary / suppress)
5. Enforce the inline comment cap
6. Apply escalation overrides

---

## Confidence Penalty

When `IndexAdapter.IsApproximate()` is `true`, apply a 0.85× multiplier to any finding whose slice included call-graph or test context:

```go
func applyApproximatePenalty(findings []core.Finding, approximate bool, eventLog func(core.Finding, string, string, string)) []core.Finding {
    if !approximate {
        return findings
    }
    for i := range findings {
        f := &findings[i]
        // Only penalize findings that used semantic context
        // (diff-only findings are not affected)
        if f.Metadata != nil && hasSemanticContext(f.Metadata) {
            oldScore := f.ConfidenceScore
            oldTier := f.ConfidenceTier
            f.ConfidenceScore = roundConfidence(f.ConfidenceScore * 0.85)
            f.ConfidenceTier = tierFromScore(f.ConfidenceScore)

            // Record the adjustment in the audit log
            eventLog(*f, "confidence_adjusted",
                fmt.Sprintf("%.4f", oldScore),
                fmt.Sprintf("%.4f", f.ConfidenceScore))
            if oldTier != f.ConfidenceTier {
                eventLog(*f, "tier_changed",
                    string(oldTier),
                    string(f.ConfidenceTier))
            }
        }
    }
    return findings
}

func tierFromScore(score float64) core.ConfidenceTier {
    switch {
    case score >= 0.80:
        return core.ConfidenceHigh
    case score >= 0.50:
        return core.ConfidenceMedium
    default:
        return core.ConfidenceLow
    }
}

// roundConfidence rounds to 4 decimal places after any arithmetic operation.
// This prevents IEEE 754 float artifacts (e.g., 0.94 * 0.85 = 0.7999...)
// from causing spurious tier transitions at boundaries.
func roundConfidence(score float64) float64 {
    return math.Round(score*10000) / 10000
}
```

This penalty is applied once, before dedup and triage. It never upgrades — only downgrades.

---

## Dedup Against Prior Findings

For a given PR, check whether each finding duplicates a prior one:

```sql
SELECT location_hash, content_hash
FROM findings
WHERE pull_request_id = $1
  AND location_hash = $2
  AND addressed_status = 'unaddressed'
ORDER BY created_at DESC
LIMIT 1;
```

**Dedup rules:**

| location_hash match | content_hash match | Action |
|--------------------|--------------------|--------|
| Yes | Yes (code unchanged) | Suppress — already flagged, nothing changed |
| Yes | No (code changed) | Allow — re-surface; code changed but same problem pattern may persist |
| No | N/A | Allow — new finding location |

```go
func shouldSuppressDuplicate(ctx context.Context, store adapter.StoreAdapter, f core.Finding, prID uuid.UUID) bool {
    prior, err := store.FindPriorFinding(ctx, prID, f.LocationHash)
    if err != nil || prior == nil {
        return false // no prior finding — allow
    }
    if f.ContentHash != nil && prior.ContentHash != nil && *f.ContentHash == *prior.ContentHash {
        return true // same location, same code — suppress
    }
    return false // same location, different code — re-surface
}
```

---

## Dismissed Fingerprints

Permanent suppression. If a team dismisses a finding, it never resurfaces for that repo.

```sql
SELECT 1 FROM dismissed_fingerprints
WHERE fingerprint = $1 AND repo_full_name = $2;
```

Check runs after dedup. A dismissed fingerprint blocks posting regardless of code changes.

---

## Triage

The `Triage` method partitions findings into three tiers:

```go
func (p *DefaultPolicy) Triage(ctx context.Context, findings []core.Finding) (inline, summary, suppress []core.Finding) {
    var candidates []core.Finding // potential inline findings
    var summaryFindings []core.Finding
    var suppressedFindings []core.Finding

    for i := range findings {
        f := &findings[i]

        // Step 1: Check permanent dismissal
        if dismissed, _ := p.store.IsFingerprintDismissed(ctx, f.LocationHash, f.RepoFullName); dismissed {
            reason := "dismissed_fingerprint"
            f.SuppressionReason = &reason
            suppressedFindings = append(suppressedFindings, *f)
            continue
        }

        // Step 2: Check dedup against prior findings
        if shouldSuppressDuplicate(ctx, p.store, *f, f.PullRequestID) {
            reason := "duplicate"
            f.SuppressionReason = &reason
            suppressedFindings = append(suppressedFindings, *f)
            continue
        }

        // Step 3: Classify by tier
        switch {
        case p.shouldEscalate(*f):
            // Escalated findings always go inline, regardless of confidence
            candidates = append(candidates, *f)

        case f.ConfidenceTier == core.ConfidenceHigh:
            candidates = append(candidates, *f)

        case f.ConfidenceTier == core.ConfidenceMedium:
            summaryFindings = append(summaryFindings, *f)

        default: // low confidence
            reason := "low_confidence"
            f.SuppressionReason = &reason
            suppressedFindings = append(suppressedFindings, *f)
        }
    }

    // Step 4: Enforce inline cap.
    // IMPORTANT: enforceInlineCap sorts candidates in-place. The overflow
    // slice (candidates[cap:]) depends on this sort having already happened,
    // so that overflow contains the lowest-priority findings.
    inline = enforceInlineCap(candidates, p.maxFindingsPerPR)
    if len(candidates) > p.maxFindingsPerPR {
        overflow := candidates[p.maxFindingsPerPR:]
        summaryFindings = append(overflow, summaryFindings...)
    }

    return inline, summaryFindings, suppressedFindings
}
```

### Inline Cap Enforcement

```go
// enforceInlineCap sorts candidates in-place by priority and returns the top `cap` entries.
// SIDE EFFECT: mutates the order of the candidates slice. The caller relies on this
// mutation to correctly identify overflow findings (candidates[cap:]).
func enforceInlineCap(candidates []core.Finding, cap int) []core.Finding {
    if len(candidates) <= cap {
        return candidates
    }

    // Sort by: escalated first, then by severity (critical > high > medium > low > info),
    // then by confidence score descending
    sort.Slice(candidates, func(i, j int) bool {
        a, b := candidates[i], candidates[j]
        if isEscalated(a) != isEscalated(b) {
            return isEscalated(a)
        }
        if severityRank(a.Severity) != severityRank(b.Severity) {
            return severityRank(a.Severity) > severityRank(b.Severity)
        }
        return a.ConfidenceScore > b.ConfidenceScore
    })

    return candidates[:cap]
}

func severityRank(s core.Severity) int {
    switch s {
    case core.SeverityCritical: return 5
    case core.SeverityHigh:     return 4
    case core.SeverityMedium:   return 3
    case core.SeverityLow:      return 2
    case core.SeverityInfo:     return 1
    default:                    return 0
    }
}
```

### Escalation Rules

```go
func (p *DefaultPolicy) shouldEscalate(f core.Finding) bool {
    // Security findings with critical or high severity always post inline
    if f.Category == core.CategorySecurity &&
        (f.Severity == core.SeverityCritical || f.Severity == core.SeverityHigh) {
        return true
    }
    return false
}
```

Default `maxFindingsPerPR`: 7. Configurable.

---

## Posting Flow

After triage, the pipeline hands findings to the ingest layer for posting:

```
1. Persist all findings (including suppressed) FIRST:
   For each finding in inline + summary + suppress:
       store.CreateFinding(ctx, &finding)
       store.CreateFindingEvent(ctx, finding.ID, "created", "mimir", "", "", {})
       if finding.SuppressionReason != nil:
           store.CreateFindingEvent(ctx, finding.ID, "suppressed", "mimir",
               "", *finding.SuppressionReason, {})

2. Post inline findings:
   For each finding in `inline`:
       commentID, err := provider.PostComment(ctx, req)
       store.MarkFindingPosted(ctx, finding.ID, commentID)
       store.CreateFindingEvent(ctx, finding.ID, "posted", "mimir",
           "", fmt.Sprintf("comment_id=%d", commentID), {})

3. Build and post summary comment:
   body := buildSummaryComment(inline, summary, failedTasks, approximate)
   commentID, err := provider.PostSummaryComment(ctx, repoFullName, prNumber, body)
   Store commentID in pull_requests.metadata["summary_comment_id"]
```

Findings are persisted **before** posting to GitHub. If posting fails (network error, rate limit), the findings are already in the database and can be retried without data loss. All findings are persisted, even suppressed ones — with their `suppression_reason` recorded — creating the audit trail for eval and allowing the reaction poller to track feedback on posted findings.

### Posting Rate-Limit Awareness

The posting loop checks `X-RateLimit-Remaining` after each `PostComment` call. If remaining drops below 100, pause for the duration indicated by `X-RateLimit-Reset`. If a 403/429 with rate-limit headers is returned, log the finding ID and continue to the next finding — the `PostingRetryJob` will pick it up.

```go
for _, f := range inline {
    commentID, err := provider.PostComment(ctx, buildCommentRequest(f))
    if err != nil {
        if isRateLimited(err) {
            log.Warn("rate limited during posting, remaining findings deferred to retry job",
                zap.String("finding_id", f.ID.String()))
            break // stop posting this run; retry job handles the rest
        }
        log.Error("failed to post finding", zap.String("finding_id", f.ID.String()), zap.Error(err))
        continue // finding is persisted; retry job will pick it up
    }
    store.MarkFindingPosted(ctx, f.ID, commentID)
    store.CreateFindingEvent(ctx, f.ID, "posted", "mimir", nil, ptr(fmt.Sprintf("comment_id=%d", commentID)))
}
```

### Posting Retry Job

A periodic River job (`PostingRetryJob`) runs every 5 minutes and recovers findings that were persisted but never posted due to GitHub API failures:

```go
// 1. Query ListUnpostedFindings (see store queries)
// 2. For each finding:
//    a. Check rate limit headroom
//    b. PostComment
//    c. MarkFindingPosted + CreateFindingEvent("posted")
//    d. On failure: log and continue to next finding
// 3. If no unposted findings remain, the job is a no-op.
```

This ensures no finding is silently lost between persist and post. The `ListUnpostedFindings` query (Spec 07) scopes to findings from completed pipeline runs within the last 7 days.

---

## Summary Comment Template

```markdown
## Mimir Review — PR #{pr_number}

**Coverage:** {reviewed_count}/{total_count} functions reviewed{failed_disclaimer}

### Inline Findings ({inline_count})
| File | Line | Category | Severity | Title |
|------|------|----------|----------|-------|
{for each inline finding: | {file} | {line} | {category} | {severity} | {title} |}

### Additional Findings ({summary_count})
| File | Symbol | Category | Confidence | Severity | Title |
|------|--------|----------|------------|----------|-------|
{for each summary finding: | {file} | {symbol} | {category} | {tier} | {severity} | {title} |}

{if failed_tasks:}
### Incomplete
{for each failed task:}
- `{file}:{symbol}` — {error_description}
{end}
{end}

---
{if approximate:}
*Context: semantic index was heuristic for this review. Caller relationships are approximate.*
{end}
*Was this review helpful? React with :+1: or :-1: on inline comments to help us improve.*
```

The `{failed_disclaimer}` is ` (N tasks failed — see below)` when any tasks failed, empty otherwise.
