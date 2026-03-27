# Spec 06: Policy — Filtering, Triage & Posting

> **Status:** M1 Build Spec
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
func applyApproximatePenalty(findings []core.Finding, approximate bool) []core.Finding {
    if !approximate {
        return findings
    }
    for i := range findings {
        f := &findings[i]
        // Only penalize findings that used semantic context
        // (diff-only findings are not affected)
        if f.Metadata != nil && hasSemanticContext(f.Metadata) {
            f.ConfidenceScore *= 0.85
            // Downgrade tier if score dropped below threshold
            f.ConfidenceTier = tierFromScore(f.ConfidenceScore)
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

    for _, f := range findings {
        // Step 1: Check permanent dismissal
        if dismissed, _ := p.store.IsFingerprintDismissed(ctx, f.LocationHash, f.RepoFullName); dismissed {
            suppressedFindings = append(suppressedFindings, f)
            continue
        }

        // Step 2: Check dedup against prior findings
        if shouldSuppressDuplicate(ctx, p.store, f, f.PullRequestID) {
            suppressedFindings = append(suppressedFindings, f)
            continue
        }

        // Step 3: Classify by tier
        switch {
        case p.shouldEscalate(f):
            // Escalated findings always go inline, regardless of confidence
            candidates = append(candidates, f)

        case f.ConfidenceTier == core.ConfidenceHigh:
            candidates = append(candidates, f)

        case f.ConfidenceTier == core.ConfidenceMedium:
            summaryFindings = append(summaryFindings, f)

        default: // low confidence
            suppressedFindings = append(suppressedFindings, f)
        }
    }

    // Step 4: Enforce inline cap
    inline = enforceInlineCap(candidates, p.maxFindingsPerPR)
    // Overflow moves to summary
    if len(candidates) > p.maxFindingsPerPR {
        overflow := candidates[p.maxFindingsPerPR:]
        summaryFindings = append(overflow, summaryFindings...)
    }

    return inline, summaryFindings, suppressedFindings
}
```

### Inline Cap Enforcement

```go
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
1. Post inline findings:
   For each finding in `inline`:
       commentID, err := provider.PostComment(ctx, req)
       store.MarkFindingPosted(ctx, finding.ID, commentID)

2. Build and post summary comment:
   body := buildSummaryComment(inline, summary, failedTasks, approximate)
   commentID, err := provider.PostSummaryComment(ctx, repoFullName, prNumber, body)
   Store commentID in pull_requests.metadata["summary_comment_id"]

3. Persist all findings (including suppressed):
   For each finding in inline + summary + suppress:
       store.CreateFinding(ctx, &finding)
```

All findings are persisted, even suppressed ones. This creates the audit trail for eval and allows the reaction poller to track feedback on posted findings.

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
