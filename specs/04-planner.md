# Spec 04: Task Planner

> **Status:** Reviewed
> **Date:** 2026-03-27
> **Package:** `internal/planner`

---

## Responsibilities

1. Generate one `ReviewTask` per changed symbol above the risk threshold
2. Assign task types (security, logic, test_coverage, style)
3. Compute risk scores
4. Route tasks to models based on task type + config
5. De-duplicate against prior tasks for the same PR

---

## Input / Output

**Input:**
- `core.PullRequest` (with diff, changed files, head/base SHAs)
- `core.SymbolTable` (from the index stage)
- Configuration: risk thresholds, model routing map, task type rules

**Output:**
- `[]core.ReviewTask` (persisted to `review_tasks` table)

---

## Task Granularity

One task per **(changed symbol, task type)** pair. A single function can produce multiple tasks if it qualifies for multiple review types.

Example: `HandleLogin` in `internal/auth/handler.go`:
- Task 1: `(HandleLogin, security)` — because it's in an auth package
- Task 2: `(HandleLogin, logic)` — because it has non-trivial control flow changes

### File-Level Tasks

For files without tree-sitter support (YAML, Dockerfile, etc.) or files where no individual symbols were changed (e.g., only comments or whitespace changed within a function):

- Create one task per file, not per symbol
- `Symbol` field is empty
- Task type defaults to `logic`
- Risk score is computed from file path patterns only

---

## Risk Scoring

Risk score is a float64 in [0.0, 1.0]. It determines whether a symbol is reviewed and influences task prioritization.

### Scoring Algorithm

```go
// computeRiskScore determines review priority for a changed symbol.
// changedLineCount is the number of diff lines that overlap the symbol's range,
// NOT the symbol's total size. This is computed by intersecting the diff hunks
// with the symbol's [StartLine, EndLine] range (see index.symbolOverlapsDiff).
func computeRiskScore(sym core.Symbol, filePath string, hasTests bool, changedLineCount int) core.RiskScore {
    score := 0.0

    // File path signals
    score += pathRisk(filePath) // 0.0–0.4

    // Symbol characteristics
    if sym.Exported {
        score += 0.1 // public API surface = higher risk
    }
    if sym.Kind == core.SymbolFunc || sym.Kind == core.SymbolMethod {
        score += 0.05 // functions are riskier than types
    }

    // Test coverage signal
    if !hasTests {
        score += 0.15 // untested code = higher risk
    }

    // Change magnitude — based on actual changed lines, not total symbol size.
    // A 200-line function with a 1-line fix should score low on this axis.
    if changedLineCount > 50 {
        score += 0.2
    } else if changedLineCount > 20 {
        score += 0.1
    }

    return core.RiskScore(min(score, 1.0))
}
```

### Path Risk Patterns

```go
var pathRiskPatterns = []struct {
    pattern string
    score   float64
}{
    {"auth",       0.4},
    {"security",   0.4},
    {"crypto",     0.4},
    {"migration",  0.35},
    {"middleware",  0.3},
    {"handler",    0.25},
    {"api",        0.25},
    {"cmd",        0.2},
    {"internal",   0.1},
    {"config",     0.2},
    {"deploy",     0.15},
    {"helm",       0.15},
    {"k8s",        0.15},
    {"kubernetes", 0.15},
}

func pathRisk(filePath string) float64 {
    best := 0.0
    for _, p := range pathRiskPatterns {
        if strings.Contains(filePath, p.pattern) && p.score > best {
            best = p.score
        }
    }
    return best
}
```

### Thresholds

| Threshold | Value | Behavior |
|-----------|-------|----------|
| `HighRiskThreshold` | 0.7 | Always reviewed, prioritized first |
| `LowRiskThreshold` | 0.3 | Skipped unless total task count is < 5 (review everything on small PRs) |

When the PR has fewer than 5 changed symbols, skip no symbols regardless of risk score. Small PRs should get full coverage.

---

## Task Type Assignment

Each changed symbol is classified into one or more task types based on heuristics.

```go
func assignTaskTypes(sym core.Symbol, filePath string) []core.TaskType {
    types := []core.TaskType{}

    // Security: auth/security paths, or exported handlers
    if isSecurityRelevant(filePath) || (sym.Exported && isHandlerLike(sym.Name)) {
        types = append(types, core.TaskTypeSecurity)
    }

    // Logic: always, unless the change is purely stylistic
    types = append(types, core.TaskTypeLogic)

    // Test coverage: if the symbol has no associated test file
    if !hasTestFile(sym.FilePath) {
        types = append(types, core.TaskTypeTestCoverage)
    }

    // Style: only for large functions or functions with many parameters
    // (style review of small functions is not worth the model call)
    if sym.EndLine-sym.StartLine > 30 {
        types = append(types, core.TaskTypeStyle)
    }

    return types
}

func isSecurityRelevant(path string) bool {
    patterns := []string{"auth", "security", "crypto", "token", "session", "password", "secret", "middleware"}
    for _, p := range patterns {
        if strings.Contains(path, p) {
            return true
        }
    }
    return false
}
```

### Multi-Task Dedup

If a symbol gets both `security` and `logic` tasks, they produce separate findings that may overlap. The policy layer handles cross-task dedup via `location_hash` — if two tasks produce findings for the same location and category, the higher-confidence one wins.

---

## Model Routing

The planner resolves which model handles each task based on a config map.

```go
// Default routing (overridable via config)
var defaultModelRouting = map[core.TaskType]string{
    core.TaskTypeSecurity:     "claude-opus-4-6",
    core.TaskTypeLogic:        "claude-opus-4-6",
    core.TaskTypeTestCoverage: "claude-sonnet-4-6",
    core.TaskTypeStyle:        "claude-haiku-4-5-20251001",
}
```

The resolved `ModelID` is stored on the `ReviewTask` so the runtime knows which `ModelAdapter` to use without re-resolving.

---

## Task-Level Dedup

On a `synchronize` event (new push to existing PR), the planner checks for prior tasks:

```sql
SELECT f.location_hash, f.content_hash
FROM review_tasks rt
JOIN findings f ON f.review_task_id = rt.id
WHERE rt.pull_request_id = $1
  AND rt.status = 'completed'
```

For each candidate task: if a prior task exists with the same `location_hash` AND the `content_hash` of the underlying code is unchanged, skip the task. The prior findings are still valid.

This prevents re-reviewing unchanged functions on each push. Only new or modified symbols get new tasks.

---

## Helm/K8s YAML Handling

Files matching `*.yaml`, `*.yml`, `*.tpl` in paths containing `helm`, `chart`, `k8s`, `kubernetes`, `deploy`, or `manifests`:

- One file-level task per changed file
- Task type: `logic` (no security/style classification for YAML)
- Risk score: from path patterns only (no symbol analysis)
- Model: follows the `logic` routing (Opus by default)
- The runtime builds a diff-only slice (no call graph, no test context)

This is a heuristic. Not all YAML files are Helm charts. But the cost of reviewing a non-Helm YAML file is one model call with a diff-only slice — low cost, and the model can still provide useful feedback on configuration changes.
