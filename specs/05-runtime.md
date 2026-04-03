# Spec 05: Runtime — Slice Building & Model Execution

> **Status:** Draft
> **Date:** 2026-03-27
> **Package:** `internal/runtime`
> **ADR:** [0005-service-architecture.md](../adr/0005-service-architecture.md) (bounded concurrency, errgroup patterns)

---

## Responsibilities

1. Build token-budgeted context slices for each `ReviewTask`
2. Assemble prompts and call `ModelAdapter.Infer`
3. Fan out task execution with bounded concurrency
4. Collect findings, handle task-level failures
5. Invoke `StaticToolAdapter` if configured (M1: no implementations, stub call site only)

---

## Slice Building

### Budget Allocation

The slice builder uses a **cap-not-allocation** model. Each section gets a maximum, but unused budget redistributes to the diff.

**Default budgets** (percentage of `TokenBudget.Total`):

| Section | Normal | Approximate Index | Style Tasks |
|---------|--------|-------------------|-------------|
| Diff hunk | 60% | 70% | 100% |
| Call graph | 25% | 17% | 0% |
| Test context | 15% | 13% | 0% |

**Per-task-type overrides** (applied on top of defaults):

| Task Type | Adjustment |
|-----------|-----------|
| security | Call graph cap +10% (from 25% → 35%), test cap -10% |
| test_coverage | Test cap +20% (from 15% → 35%), call graph cap -20% |
| logic | Default split |
| style | Diff only |

### Fill Algorithm

```go
func buildSlice(ctx context.Context, task core.ReviewTask, index adapter.IndexAdapter, budget core.TokenBudget) (core.Slice, error) {
    // 1. Get available content from the index
    indexResult, err := index.Query(ctx, adapter.IndexRequest{
        RepoPath:    task.RepoPath,
        ChangedFile: task.FilePath,
        Symbol:      task.Symbol,
        Budget:      budget,
    })
    if err != nil {
        return core.Slice{}, fmt.Errorf("index query: %w", err)
    }

    // 2. Fill in priority order: diff → call graph → tests
    slice := core.Slice{
        Approximate: index.IsApproximate(),
    }

    // Diff hunk: always present, fill up to cap
    diffTokens := min(tokenCount(indexResult.DiffHunk), budget.DiffHunk)
    slice.DiffHunk = truncateToTokens(indexResult.DiffHunk, diffTokens)
    remainder := budget.Total - diffTokens

    // Call graph: fill up to cap, unused → remainder
    callGraphTokens := min(tokenCount(indexResult.CallGraph), budget.CallGraph, remainder)
    slice.CallGraph = truncateToTokens(indexResult.CallGraph, callGraphTokens)
    remainder -= callGraphTokens

    // Test context: fill up to cap, unused → remainder
    testTokens := min(tokenCount(indexResult.TestContext), budget.Tests, remainder)
    slice.TestContext = truncateToTokens(indexResult.TestContext, testTokens)
    remainder -= testTokens

    // If remainder > 0 and diff was truncated, give it back to diff
    if remainder > 0 && tokenCount(indexResult.DiffHunk) > diffTokens {
        extra := min(tokenCount(indexResult.DiffHunk)-diffTokens, remainder)
        slice.DiffHunk = truncateToTokens(indexResult.DiffHunk, diffTokens+extra)
    }

    // Mark truncation
    if tokenCount(indexResult.DiffHunk) > budget.Total {
        slice.Truncated = true
    }

    return slice, nil
}
```

### Token Counting (M1)

Character estimate with 20% safety margin:

```go
func tokenCount(s string) int {
    // 1 token ≈ 4 characters, with 20% safety margin
    // Effective: 1 token ≈ 3.33 characters
    return int(math.Ceil(float64(len(s)) / 3.33))
}
```

Revisit if context limit errors appear in practice. M2 can integrate model-specific tokenizers.

### Default Token Budgets

```go
var defaultBudgets = map[string]int{
    "claude-opus-4-6":          16_000, // cap per task, not full context window
    "claude-sonnet-4-6":        16_000,
    "claude-haiku-4-5-20251001": 8_000,
}
```

The per-task cap is deliberately conservative. For Opus with 200K context, 16K per task allows ~12 parallel tasks before the model's total context is a concern. In practice, each task is an independent API call, so context windows don't compete — but keeping slices focused improves finding quality.

---

## Prompt Assembly

See `specs/12-prompts.md` for full templates. The runtime assembles:

```
[System prompt]              — persona, constraints, output schema
[Task framing]               — task type, file path, symbol name
[Approximate context note]   — if IsApproximate(), inject warning
[Slice: diff hunk]           — labeled "## Changed Code"
[Slice: call graph]          — labeled "## Callers and Callees" (if non-empty)
[Slice: test context]        — labeled "## Related Tests" (if non-empty)
[Truncation notice]          — if slice was truncated
[Output instructions]        — JSON schema for structured response
```

---

## Structured Output Schema

The model returns findings as JSON:

```json
{
  "findings": [
    {
      "title": "SQL injection via unsanitized user input",
      "body": "The `query` parameter is interpolated directly into...",
      "suggestion": "Use parameterized queries: db.Query(ctx, sql, args...)",
      "category": "security",
      "severity": "critical",
      "confidence_tier": "high",
      "confidence_score": 0.92,
      "start_line": 42,
      "end_line": 45
    }
  ]
}
```

The runtime validates each finding:
1. `confidence_tier` must match `confidence_score` (via `core.ValidateConfidenceTier`)
2. `category` must be a valid `FindingCategory`
3. `severity` must be a valid `Severity`
4. `start_line` and `end_line` must be within the diff hunk range
5. If validation fails, log the invalid finding and skip it (do not crash the task)

---

## Bounded Concurrency

From ADR-0005 review notes:

```go
func executeTasks(ctx context.Context, tasks []core.ReviewTask, deps RuntimeDeps) []core.Finding {
    var mu sync.Mutex
    var allFindings []core.Finding

    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(deps.Config.MaxConcurrentTasks) // default: 10

    for _, task := range tasks {
        g.Go(func() error {
            taskCtx, cancel := context.WithTimeout(ctx, deps.Config.PerTaskTimeout) // default: 60s
            defer cancel()

            // Update task status to running
            deps.Store.UpdateReviewTaskStatus(taskCtx, task.ID, string(core.TaskStatusRunning), nil)

            findings, err := executeOneTask(taskCtx, task, deps)
            if err != nil {
                // Record failure, do NOT return error (would cancel siblings)
                errStr := err.Error()
                deps.Store.UpdateReviewTaskStatus(ctx, task.ID, string(core.TaskStatusFailed), &errStr)
                return nil
            }

            deps.Store.UpdateReviewTaskStatus(ctx, task.ID, string(core.TaskStatusCompleted), nil)

            mu.Lock()
            allFindings = append(allFindings, findings...)
            mu.Unlock()
            return nil
        })
    }

    g.Wait()
    return allFindings
}
```

### Circuit Breaker

When multiple tasks fail consecutively due to provider errors, a circuit breaker prevents wasting budget and time hammering a dead endpoint:

```go
type circuitBreaker struct {
    mu              sync.Mutex
    consecutiveFails int
    threshold        int  // default: 3
    tripped          bool
}

func (cb *circuitBreaker) recordFailure(err error) {
    if !isProviderError(err) {
        return // only trip on provider-side errors, not validation/parse errors
    }
    cb.mu.Lock()
    defer cb.mu.Unlock()
    cb.consecutiveFails++
    if cb.consecutiveFails >= cb.threshold {
        cb.tripped = true
    }
}

func (cb *circuitBreaker) recordSuccess() {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    cb.consecutiveFails = 0
    cb.tripped = false
}

func (cb *circuitBreaker) isOpen() bool {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    return cb.tripped
}
```

In `executeTasks`, check the breaker before launching each task:

```go
if breaker.isOpen() {
    errStr := "circuit breaker open: provider appears unhealthy"
    deps.Store.UpdateReviewTaskStatus(ctx, task.ID, string(core.TaskStatusFailed), &errStr)
    return nil
}
```

The breaker resets on the first successful inference call. It is scoped to a single pipeline run (not global), so a transient provider outage doesn't permanently block future runs.

### Task-Level Retry

Inside `executeOneTask`, LLM calls get one retry:

```go
func executeOneTask(ctx context.Context, task core.ReviewTask, deps RuntimeDeps) ([]core.Finding, error) {
    slice, err := buildSlice(ctx, task, deps.Index, resolveBudget(task, deps.Config))
    if err != nil {
        return nil, fmt.Errorf("build slice: %w", err)
    }

    prompt := assemblePrompt(task, slice, deps.Config)

    findings, err := deps.Model.Infer(ctx, task, slice)
    if err != nil {
        if isRetryable(err) {
            // One retry with backoff
            select {
            case <-ctx.Done():
                return nil, ctx.Err()
            case <-time.After(2 * time.Second):
            }
            findings, err = deps.Model.Infer(ctx, task, slice)
            if err != nil {
                return nil, fmt.Errorf("model inference (retry): %w", err)
            }
        } else {
            return nil, fmt.Errorf("model inference: %w", err)
        }
    }

    return findings, nil
}

func isRetryable(err error) bool {
    // 429 (rate limit), 500, 502, 503, 529 are retryable
    // 400, 401, 403, 404 are not (bad request = bug)
    var apiErr *core.APIError
    if errors.As(err, &apiErr) {
        return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
    }
    // Network errors and timeouts are retryable
    return errors.Is(err, context.DeadlineExceeded) || isNetworkError(err)
}
```

---

## Static Tool Stub

M1 ships no `StaticToolAdapter` implementations, but the call site exists:

```go
// In executeOneTask, after slice building and before model inference:
if deps.StaticTools != nil {
    for _, tool := range deps.StaticTools {
        if !slices.Contains(tool.Languages(), filepath.Ext(task.FilePath)) {
            continue
        }
        staticFindings, err := tool.Run(ctx, task, []string{task.FilePath})
        if err != nil {
            // Log and continue — static tool failure is non-fatal
            log.Warn("static tool failed", zap.String("tool", tool.ToolName()), zap.Error(err))
            continue
        }
        // Static tool findings are added to the slice as grounding context
        // AND collected as findings in their own right
        allFindings = append(allFindings, staticFindings...)
    }
}
```

This call site is a no-op when `deps.StaticTools` is empty (the M1 default). M2 plugs in semgrep/golangci-lint without runtime changes.

---

## Within-Run Dedup

Multiple tasks for the same PR may produce findings for the same location (e.g., a `security` and `logic` task both flag the same line). Before returning findings to the policy layer:

```go
// deduplicateResult holds the winners (to post/triage) and losers (to persist with
// suppression reason for audit). No finding is silently dropped.
type deduplicateResult struct {
    kept       []core.Finding
    suppressed []core.Finding // losers: same location, lower confidence
}

func deduplicateFindings(findings []core.Finding) deduplicateResult {
    seen := make(map[string]int) // location_hash → index of best finding
    loserIndices := make(map[int]bool)

    for i, f := range findings {
        if prev, ok := seen[f.LocationHash]; ok {
            // Keep the one with higher confidence; mark loser for suppression
            if f.ConfidenceScore > findings[prev].ConfidenceScore {
                loserIndices[prev] = true
                delete(loserIndices, i)
                seen[f.LocationHash] = i
            } else {
                loserIndices[i] = true
            }
        } else {
            seen[f.LocationHash] = i
        }
    }

    // Collect winners and sort by confidence descending for deterministic output.
    // Map iteration order in Go is random; without sorting, identical inputs
    // could produce differently-ordered outputs across runs, which breaks
    // eval reproducibility and makes the summary comment order non-deterministic.
    indices := make([]int, 0, len(seen))
    for _, idx := range seen {
        indices = append(indices, idx)
    }
    sort.Slice(indices, func(i, j int) bool {
        return findings[indices[i]].ConfidenceScore > findings[indices[j]].ConfidenceScore
    })
    kept := make([]core.Finding, 0, len(indices))
    for _, idx := range indices {
        kept = append(kept, findings[idx])
    }

    // Mark losers with suppression reason so they are persisted for audit.
    suppressed := make([]core.Finding, 0, len(loserIndices))
    for idx := range loserIndices {
        f := findings[idx]
        reason := "duplicate"
        f.SuppressionReason = &reason
        suppressed = append(suppressed, f)
    }

    return deduplicateResult{kept: kept, suppressed: suppressed}
}
```

### Suppression Visibility

Suppressed findings are persisted to the database and surfaced to end users in two ways:

1. **Summary comment.** The PR summary comment includes a "Suppressed Findings" section showing counts by reason (duplicate, low confidence, dismissed fingerprint) so reviewers know what was filtered and why.
2. **CLI output.** `mimir review --show-suppressed` includes suppressed findings in the output table, annotated with their suppression reason.

Suppressed findings may contain critical information — especially duplicates where the underlying issue persists across pushes. Full visibility ensures nothing is silently hidden.
