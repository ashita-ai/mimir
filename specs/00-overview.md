# Spec 00: Pipeline Architecture & M1 Scope

> **Status:** Draft
> **Date:** 2026-03-27

---

## Pipeline DAG

```
GitHub Webhook (pull_request: opened | synchronize | reopened)
        │
        ▼
[ingest] ReceiveWebhook
        ├── Verify X-Hub-Signature-256
        ├── Parse payload → core.PREvent
        └── Enqueue River job (transactional with event persistence)
        │
        ▼
[ingest] FetchPR (inside River job handler)
        ├── Exchange GitHub App JWT → installation token
        ├── GET /repos/{owner}/{repo}/pulls/{number}
        ├── GET diff (Accept: application/vnd.github.v3.diff)
        └── Output: core.PullRequest (normalized metadata + diff + file list)
        │
        ▼
[index] CheckoutRepo
        ├── Shallow blobless clone to temp directory (--filter=blob:none)
        ├── Checkout head_sha
        ├── Cleanup deferred to end of pipeline run
        └── Output: repoPath (local directory path for tree-sitter)
        │
        ▼
[store] CreatePipelineRun
        ├── Record: PR ID, head_sha, prompt_version, config_hash
        ├── Status: 'running'
        └── Output: core.PipelineRun with ID (threaded through all downstream stages)
        │
        ▼
[index] BuildSymbolTable
        ├── git diff --name-only base_sha..head_sha → changed_files
        ├── For each changed file: tree-sitter parse → extract symbols
        ├── Find importer files (package import filter)
        ├── For each importer: tree-sitter parse → extract references
        └── Output: core.SymbolTable (ephemeral, in-memory)
        │
        ▼
[planner] GenerateTasks
        ├── For each changed symbol above risk threshold:
        │   ├── Assign task type(s): security | logic | test_coverage | style
        │   ├── Compute risk score
        │   └── Route to model based on task type + config
        ├── De-duplicate against prior tasks (same PR, same location_hash, same content_hash)
        └── Output: []core.ReviewTask (persisted to DB)
        │
        ▼
[runtime] ExecuteTasks (bounded errgroup fan-out)
        ├── For each task (parallel, max N concurrent):
        │   ├── Build token-budgeted Slice (diff + call graph + tests)
        │   ├── Assemble prompt (system + task framing + slice + output schema)
        │   ├── Call ModelAdapter.Infer → []core.Finding
        │   ├── On failure: record error in review_tasks, continue siblings
        │   └── On success: collect findings
        └── Output: []core.Finding (aggregated across tasks)
        │
        ▼
[policy] Triage
        ├── Apply confidence penalties (approximate index: ×0.85)
        ├── Dedup against prior findings (location_hash + content_hash)
        ├── Check dismissed_fingerprints
        ├── Partition: inline (≤7, high confidence) | summary | suppress
        ├── Escalation overrides (security/critical → always inline)
        └── Output: inline []Finding, summary []Finding, suppress []Finding
        │
        ▼
[store] PersistFindings (BEFORE posting — findings survive GitHub API failures)
        ├── Write all findings (including suppressed) to DB with fingerprints
        ├── Record suppression_reason for each suppressed finding
        ├── Emit 'created' finding_event for each finding
        ├── Emit 'suppressed' finding_event for each suppressed finding
        └── Output: []core.Finding with IDs
        │
        ▼
[ingest] PostResults
        ├── PostComment for each inline finding
        ├── PostSummaryComment with full review table
        ├── MarkFindingPosted for each posted finding (+ 'posted' finding_event)
        ├── Check addressed_in_next_commit for prior findings (content-hash diff)
        └── CompletePipelineRun with final task/finding counts
```

---

## Package Dependency Graph

```
cmd/mimir
  ├── pkg/adapter          (interface types only)
  ├── internal/core         (domain types only, no I/O)
  ├── internal/ingest       (imports: core, adapter, store)
  ├── internal/index        (imports: core, adapter)
  ├── internal/planner      (imports: core, adapter, store)
  ├── internal/runtime      (imports: core, adapter, store)
  ├── internal/policy       (imports: core, adapter, store)
  ├── internal/eval         (imports: core, store)
  └── internal/store        (imports: core)
```

**Build order:** `core` → `adapter` → `store` → everything else → `cmd/mimir`.

No package in `internal/` imports another `internal/` package except through interfaces defined in `pkg/adapter`. The wiring layer in `cmd/mimir` constructs concrete implementations and injects them.

---

## M1 Scope Table

| Feature | M1 | Notes |
|---------|-----|-------|
| GitHub App authentication | Yes | JWT + installation token exchange |
| PAT fallback (CLI mode) | Yes | Env var, used when App credentials absent |
| Webhook receiver (chi) | Yes | `pull_request` events only |
| River job queue | Yes | Single job type: `ReviewPipelineJob` |
| Tree-sitter index (Go) | Yes | Full support: imports, symbols, test mapping |
| Tree-sitter index (Python) | Yes | Approximate: import analysis best-effort |
| Tree-sitter index (TypeScript) | Yes | Approximate: ES imports, barrel exports partial |
| Tree-sitter index (PHP) | Yes | Approximate: PSR-4 use statements |
| Helm/K8s YAML | Yes | Diff-only, no semantic index |
| Task planner | Yes | One task per changed symbol, risk scoring |
| Model routing per task type | Yes | Config map: task_type → model_id |
| Slice budgeting | Yes | Cap-not-allocation, dynamic redistribution |
| Anthropic ModelAdapter | Yes | Claude Opus default |
| Two-tier posting | Yes | Inline (≤7) + summary comment |
| Dual-hash fingerprinting | Yes | location_hash + content_hash dedup |
| `addressed_in_next_commit` | Yes | Content-hash re-check (Option B), TEXT column |
| Reaction-based feedback | Yes | Poll GitHub reactions, write finding_events |
| Pipeline run audit records | Yes | Every execution creates a `pipeline_runs` row |
| Append-only finding event log | Yes | All lifecycle transitions recorded in `finding_events` |
| `mimir serve` | Yes | HTTP + River workers, dual mode |
| `mimir review` | Yes | One-shot CLI, stdout output |
| Static tool adapters | Interface only | No M1 implementations |
| Semgrep integration | No | M2 |
| golangci-lint integration | No | M2 |
| Helm-aware static tooling | No | M2 |
| LSP-based index (gopls) | No | M2 |
| Replay/eval harness | No | M2 (capture data in M1) |
| Multi-turn inference | No | M2 |
| GitLab adapter | No | M2+ |
| YAML policy config | No | M2+ |
| River UI | No | M2 |
| Prometheus/OpenTelemetry | No | M2 |

---

## Concurrency Model

Three levels of concurrency, from outer to inner:

1. **River workers** (process-level). Multiple `mimir serve` instances compete for jobs via `SKIP LOCKED`. Each instance runs a configurable number of worker goroutines. Default: 5.

2. **errgroup fan-out** (job-level). Within a single pipeline run, the runtime fans out N `ReviewTask` executions. Bounded by `errgroup.SetLimit(maxConcurrentTasks)`. Default: 10. Task failures are isolated — recorded in DB, siblings continue.

3. **Context timeouts** (task-level). Each task gets `context.WithTimeout(ctx, perTaskTimeout)`. Default: 60 seconds. LLM calls, tree-sitter parsing, and GitHub API calls all respect this context.

**Cancellation flow:** River cancels the job context on timeout or shutdown → errgroup respects parent context → individual tasks see context cancellation and return.

4. **Circuit breaker** (provider-level). Within a pipeline run, if 3 consecutive tasks fail due to provider errors (429, 5xx, network), the breaker trips and remaining tasks are immediately failed with `"circuit breaker open"`. Resets on the first successful inference. Prevents wasting budget and time against a dead provider.

---

## Error Taxonomy

| Error Class | Examples | Retry? | Behavior |
|-------------|----------|--------|----------|
| **Config** | Missing API key, invalid DB URL | No | Fatal at startup, exit 1 |
| **Auth** | Expired GitHub token, invalid webhook signature | No | Reject request, log error |
| **Transient infra** | DB connection lost, network timeout | Yes (River job-level) | River re-enqueues entire job, exponential backoff, max 3 retries |
| **LLM transient** | 429 rate limit, 500 server error, timeout | Yes (task-level) | 1 retry with backoff. On second failure: mark task failed, continue siblings |
| **LLM permanent** | 400 bad request, 401 unauthorized | No | Mark task failed immediately, continue siblings |
| **Parse** | Tree-sitter can't parse file | No | Skip file in index, log warning, `IsApproximate()` → true |
| **Pipeline logic** | No changed symbols found, empty diff | No | Complete job with zero findings, post summary comment noting "no reviewable changes" |

---

## Data Durability Invariants

These invariants hold across all pipeline stages:

1. **No `ON DELETE CASCADE`.** All foreign keys use `ON DELETE RESTRICT`. Audit data is never silently destroyed.
2. **Persist before post.** Findings are written to the database before being posted to GitHub. If posting fails, findings survive for retry.
3. **Append-only audit log.** Every finding state transition (creation, posting, suppression, confidence adjustment, addressing, dismissal) is recorded in `finding_events`. The finding row captures current state; the events table captures history.
4. **Soft deletes only.** `pull_requests` has a `deleted_at` column. Hard deletion requires explicit reverse-dependency-order operations.
5. **Pipeline runs are the audit anchor.** Every pipeline execution creates a `pipeline_runs` row with config hash, prompt version, final counts, and the full model routing snapshot in `metadata`. The config hash enables quick equality checks; the `metadata` JSONB preserves the complete configuration so that no pipeline run's parameters are irrecoverable. This is the primary record for eval reproducibility.
6. **Suppression reasons are recorded.** Every suppressed finding includes a `suppression_reason` ('duplicate', 'low_confidence', 'dismissed_fingerprint') for triage auditability.
7. **No silent drops.** Within-run dedup losers are persisted with `suppression_reason = 'duplicate'`, not discarded. Every finding the model produces is recorded — winners and losers alike.
8. **Posting is retried.** Findings persisted but not posted (due to GitHub API failure) are recovered by a periodic `PostingRetryJob`. No finding is silently lost between persist and post.
