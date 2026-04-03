# ADR-0005: Service Architecture — Single Binary, Dual Runtime Mode

> **Status:** Accepted
> **Original decision date:** 2026-03-18. Significant design additions 2026-03-21. Accepted 2026-03-27.

---

## Context

Mimir in service mode requires two runtime roles:

1. **Webhook receiver** — an HTTP server that accepts GitHub events and enqueues review jobs
2. **Pipeline workers** — background goroutines that dequeue jobs, run the review pipeline, and post results

These could be separate binaries/processes, or they could be co-located in a single binary.

---

## Decision

**Single binary with two runtime modes, both started by `mimir serve`.**

`mimir serve` starts:
- A `chi` HTTP server on a configurable port (webhook receiver, health check, metrics endpoint)
- One or more `river` worker pools (pipeline workers) in the same process

Both are started together by default. Worker concurrency is configurable via flags/env.

The `mimir review` subcommand provides a standalone CLI path for one-shot PR review without a running server (useful for local development and CI integration).

---

## Consequences

**Positive:**
- One binary to build, one Docker image to ship, one process to monitor in simple deployments
- Webhook receiver is stateless (just enqueues jobs) — horizontal scaling is `docker run` more instances against the same DB
- Workers compete via `SKIP LOCKED` — no coordination required between instances
- Local dev is `mimir serve` — no separate worker process to start

**Scaling path (no architectural changes required):**
- To scale workers independently: run additional `mimir serve --http=false` instances (workers only, no HTTP)
- To scale HTTP independently: run `mimir serve --workers=0` (HTTP only, no workers) behind a load balancer
- Both modes share the same binary and config — no new artifact required

**Negative / Accepted trade-offs:**
- In-process failure of the HTTP server takes down workers and vice versa. Mitigated by: river's at-least-once delivery (in-flight jobs restart on next worker), and the fact that `chi` HTTP handling is extremely stable
- Not appropriate for very high webhook volume where HTTP and worker pools would need to scale at different ratios. Out of scope for M1/M2.

**Rejected alternatives:**
- Separate worker binary: More operational complexity (two images, two deployments) with no M1/M2 benefit. Can be introduced later without breaking the `mimir serve` path.
- Message broker (Kafka, RabbitMQ) between HTTP and workers: Adds infrastructure. River on PostgreSQL (ADR-0003) provides equivalent at-least-once semantics without a separate broker.

---

## Review Notes (2026-03-21)

### Bounded Concurrency in Runtime Fan-Out

The pipeline fans out N `ReviewTask` executions in parallel (one per changed function). This fan-out must be bounded.

```go
// Correct: bounded concurrency with task isolation
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(maxConcurrentTasks) // e.g., 10

for _, task := range tasks {
    g.Go(func() error {
        result, err := executeTask(ctx, task)
        if err != nil {
            // Record failure but do NOT return error —
            // returning error cancels all sibling goroutines.
            store.UpdateReviewTaskStatus(ctx, task.ID, "failed", &err)
            return nil
        }
        collectFindings(result)
        return nil
    })
}

// All tasks complete (or fail individually). No task kills its siblings.
g.Wait()
```

**Key:** `errgroup`'s default behavior cancels all goroutines on the first error return. We must swallow task errors and record them in the DB. The pipeline continues with partial results. The summary comment discloses incomplete coverage (see Two-Tier Posting below).

### Two-Tier Posting Strategy

Posting all findings as inline comments creates noise. The `PostSummaryComment` method in `ProviderAdapter` exists but had no design.

**Proposed strategy:**

| Finding tier | Posting behavior |
|-------------|-----------------|
| **High confidence** (≥ 0.80) | Inline comment on the relevant diff line |
| **Medium confidence** (0.50–0.79) | Summary comment only (table row, not inline) |
| **Low confidence** (< 0.50) | Suppressed entirely, unless escalation criteria met |
| **Escalated** (security/critical, any confidence) | Inline comment regardless of tier |

**Inline comment cap:** Maximum 7 inline comments per PR. If more than 7 high-confidence findings exist, the top 7 by severity are posted inline; the rest move to the summary comment.

**Summary comment structure:**

```markdown
## Mimir Review — PR #123

**Coverage:** 8/10 functions reviewed (2 tasks failed — see below)

### Inline Findings (3)
| File | Line | Category | Title |
|------|------|----------|-------|
| ... | ... | ... | ... |

### Additional Findings (5)
| File | Symbol | Category | Confidence | Title |
|------|--------|----------|------------|-------|
| ... | ... | ... | medium | ... |

### Incomplete
- `internal/foo/bar.go:HandleRequest` — LLM inference timed out (task failed)
- `internal/foo/baz.go:Validate` — LLM returned 500 (task failed)

---
*Context: semantic index was heuristic for this review. Caller relationships are approximate.*
```

This requires a new method on `PolicyAdapter`:

```go
// Triage partitions findings into posting tiers.
// Returns: inline (posted as diff comments), summary (included in summary table only),
// and suppress (not posted at all).
Triage(ctx context.Context, findings []core.Finding) (inline, summary, suppress []core.Finding)
```

This replaces the per-finding `ShouldPost` / `ShouldEscalate` pattern with a batch operation that can enforce the inline cap and prioritize across findings. The per-finding methods remain for simpler policy implementations that don't need batch logic.

### Feedback Loop for Eval

The success metrics (< 20% FP rate, ≥ 80% recall) require labeled data. The design has no ingest path for this signal.

**M1 minimum viable feedback:**

1. When Mimir posts an inline comment, include a footer: `"Was this helpful? React with :+1: or :-1:"`.
2. A lightweight webhook handler (or periodic poll) checks GitHub reactions on Mimir's comments.
3. Store reactions as `finding_events` tied to the finding's `location_hash`.
4. For M1 eval: "findings with :-1: reactions" = false positives. "Findings on PRs that merged without :-1:" = presumed true positives (weak label but free).

This is cheap to build, requires no new infrastructure, and produces the labeled dataset needed to validate the token budget split and confidence thresholds empirically.
