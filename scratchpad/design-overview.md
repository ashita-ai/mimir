# Design Overview (In Progress)

> **Status:** Draft — captures planning session output. Not an ADR. Items marked [OPEN] are unresolved.

---

## Problem Statement

Automated code review tools today fall into two camps:

1. **Static linters** — fast, precise, but limited to syntactic/stylistic issues. They don't reason about intent, architecture, or test coverage relative to risk.
2. **LLM-based reviews** — capable of reasoning, but typically dump raw diffs into a prompt. Context window limits mean they miss cross-file relationships; lack of memory means each PR is reviewed in isolation.

Mimir's thesis: a structured pipeline that builds a semantic context slice *per changed function* — not per PR — and routes that slice through a model with explicit task framing (security review, logic review, test coverage review) produces higher-signal, lower-noise findings than a "send the whole diff" approach.

---

## Pipeline DAG

```
GitHub Webhook (PR opened / synchronize)
        │
        ▼
[ingest] FetchPR → normalize PR metadata, diff, commit list
        │
        ▼
[index] BuildRepoMap → tree-sitter parse, heuristic call graph, test file mapping
        │
        ▼
[planner] GenerateTasks → one ReviewTask per changed function/symbol above risk threshold
        │
        ▼
[runtime] ExecuteTasks (fan-out) → for each task:
        ├── SliceBudget → allocate token budget (diff, call graph, tests)
        ├── RouteModel → select model based on task type + budget
        ├── RunStaticTools → semgrep, golangci-lint on relevant files
        └── Infer → LLM call → []Finding
        │
        ▼
[policy] FilterFindings → confidence threshold, dedup, escalation rules
        │
        ▼
[store] PersistFindings → write to DB with fingerprint hash
        │
        ▼
[ingest] PostComments → GitHub PR comment via API
```

---

## Module Responsibilities

### `internal/core`
Domain types only. No I/O, no external dependencies.

```go
type PullRequest struct { ... }
type ReviewTask struct { ... }
type Finding struct { ... }
type Slice struct { ... }      // token-budgeted context window
type RiskScore float64
type ConfidenceTier string     // "high" | "medium" | "low"
```

### `internal/ingest`
GitHub API interaction: fetch PR metadata, diff, file list, commit list. Normalize into `core.PullRequest`. Post findings as review comments.

### `internal/index`
Build and query the semantic repo map. M1: tree-sitter based (see ADR-0004). Returns `[]Slice` for a given `IndexRequest`.

### `internal/planner`
Given a `PullRequest` and repo map, generate `[]ReviewTask`. Each task targets one logical unit (function, type, migration). Assigns risk score and task type (security/logic/test-coverage/style).

### `internal/runtime`
Execute a `ReviewTask`. Orchestrates: slice budgeting → static tool execution → model inference → `[]Finding`. Uses `errgroup` for parallel tool execution.

### `internal/policy`
Filter and gate findings before posting. Implements confidence threshold (don't post low-confidence findings unless escalation criteria met), deduplication (fingerprint match against prior findings), and escalation (high-confidence security findings always post, regardless of posting cadence limits).

### `internal/eval`
Metrics collection, scorecard generation, replay. Enables offline evaluation of model/policy changes against a captured dataset of prior reviews.

### `internal/store`
DB layer: sqlc-generated query functions + goose migrations. One package, not scattered. All DB access goes through this package.

---

## Plugin Interface Contracts

See `scratchpad/plugin-interfaces.md` for draft Go interface definitions. The six adapter types are:

| Interface | Implementations |
|-----------|----------------|
| `ProviderAdapter` | GitHub (M1), GitLab (M2+) |
| `ModelAdapter` | OpenAI, Anthropic, Gemini (M1: Anthropic) |
| `StaticToolAdapter` | semgrep, golangci-lint, custom |
| `IndexAdapter` | tree-sitter (M1), LSP (M2) |
| `PolicyAdapter` | default policy, custom YAML policy (M2) |
| `StoreAdapter` | PostgreSQL (only) |

---

## Success Metrics (M1)

- **Precision:** < 20% false positive rate on findings (measured via reviewer dismissal rate in pilot)
- **Recall:** Captures ≥ 80% of findings a human reviewer would flag as important (measured via escape analysis on sampled PRs)
- **Latency:** P95 review completion < 90 seconds for a median PR (< 200 changed lines, < 10 changed functions)
- **Stability:** Zero dropped jobs in a 7-day pilot run (river at-least-once + retry should handle transient failures)

---

## Open Questions

### [OPEN] Finding Fingerprinting
How do we assign a stable fingerprint to a finding across commits?

Line-number-based fingerprinting breaks when code moves. Options:
1. **Content hash of the code block** — hash the AST subtree or normalized source of the flagged region. Survives line movement, breaks on edits.
2. **Symbol + file + description hash** — `sha256(function_name + file_path + finding_category + description_normalized)`. Survives line movement and minor edits, but may miss re-introduced identical issues.
3. **Semantic embedding similarity** — compare finding embeddings; "same" if cosine similarity > threshold. Accurate but slow and requires embedding infrastructure.

M1 recommendation: option 2 with a tombstone table for manually dismissed fingerprints. Revisit with option 3 in M2 eval pass.

### [OPEN] Dedup Strategy
If the same logical finding recurs across multiple review runs (e.g., a long-lived PR that keeps getting more commits), should we:
- (a) Post a new comment each time (noisy)
- (b) Edit the existing comment (requires tracking comment IDs, GitHub API write)
- (c) Suppress if fingerprint is already posted and unresolved (silent — reviewer may miss it)

Leaning toward (c) with a TTL: suppress if fingerprint was posted in the last N commits, re-surface if the code has materially changed (content hash differs from the fingerprinted version).

### [OPEN] Token Budget Validation
See `scratchpad/slice-budgeting.md`. The 60/25/15 split is a placeholder. Needs empirical validation against real review runs before being codified as policy.

### [OPEN] `addressed_in_next_commit` Detection
A finding's `addressed_in_next_commit` field should flip to `true` when a subsequent commit resolves the issue. Detection strategy:
- Re-run the index + inference on the changed region in the next commit
- If the fingerprint is no longer generated, mark as addressed
- Risk: false negatives if the model changes opinion, false positives if the fix is superficial

Alternative: require the PR author to manually resolve (GitHub review comment resolution). Less automated but more accurate.
