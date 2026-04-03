# Frequently Asked Questions

## General

### What is Mimir?

Mimir is an open-source AI PR review harness. It builds a semantic context slice *per changed function* — not per PR — and routes each slice through configurable model and static analysis pipelines. The goal is high-signal, low-noise code review findings that developers actually act on.

### How is this different from other AI code review tools?

Most AI review tools dump the entire diff into a language model and hope for the best. That approach hits context window limits on large PRs, produces vague feedback because the model lacks surrounding context, and generates the same stale comment on every push.

Mimir takes a structured approach:

1. **Per-function slicing.** Each changed function gets its own token-budgeted context window containing the diff, call graph neighbors, and relevant tests — nothing else.
2. **Task-typed review.** Each slice is reviewed through a specific lens (security, logic, test coverage, style) rather than a generic "review this code" prompt.
3. **Deterministic deduplication.** Findings are fingerprinted with dual hashes (location + AST content) so the same issue is never posted twice unless the underlying code changes.
4. **Hybrid pipeline.** Static analysis tools (semgrep, golangci-lint) run alongside model inference. Deterministic checks don't burn tokens.

### What's the current status?

Early development — the M1 milestone is in progress. The service skeleton works: webhook reception, PostgreSQL-backed job queue, database migrations, and PR persistence are functional and tested. The review pipeline stages (semantic index, task planner, model runtime, policy engine) are defined but not yet implemented.

### What's the license?

Apache 2.0.

## Architecture

### Why Go?

Three reasons (see [ADR-0001](adr/0001-language-go.md)):

- **Concurrency model.** Goroutines and `errgroup` make fan-out review tasks natural — run security, logic, and style checks in parallel with bounded concurrency.
- **Single static binary.** No runtime dependencies, no virtualenvs. `CGO_ENABLED=0 go build` produces a binary that drops into a distroless Docker image.
- **Context propagation.** Go's `context.Context` enforces cancellation and deadline budgets across the entire pipeline, which is how we target P95 < 90s latency.

### Why PostgreSQL only? Why not SQLite for local dev?

One database, one behavior, zero "works on my machine" bugs (see [ADR-0002](adr/0002-database-postgresql.md)).

PostgreSQL provides JSONB for flexible finding metadata, arrays, and `SKIP LOCKED` for the job queue — none of which have SQLite equivalents. Docker Compose starts a local PostgreSQL instance on port 5433 in one command. The operational cost of requiring Docker is far lower than the engineering cost of maintaining two storage backends.

### Why River instead of Redis, RabbitMQ, or SQS?

River is a PostgreSQL-backed job queue (see [ADR-0003](adr/0003-job-queue-river.md)). Using it means:

- **No additional infrastructure.** The database *is* the queue.
- **Transactional job insertion.** Webhook reception and job enqueue happen in a single database transaction — no "accepted the webhook but lost the job" failure mode.
- **Horizontal scaling.** Multiple `mimir serve` instances compete for jobs via `SELECT ... FOR UPDATE SKIP LOCKED`. No leader election, no coordination.
- **At-least-once delivery.** Failed jobs retry with exponential backoff. Transient LLM API errors retry within the job; infrastructure failures (OOM, crash) retry at the queue level.

### How does the review pipeline work?

```
Webhook → FetchPR → BuildRepoMap → GenerateTasks → ExecuteTasks → FilterFindings → PostComments
```

1. **FetchPR** normalizes PR metadata and diff from GitHub.
2. **BuildRepoMap** parses changed files with tree-sitter, walks one level of importers (change cone), and builds a per-run symbol table.
3. **GenerateTasks** creates one `ReviewTask` per changed function above a risk threshold, assigning a task type (security, logic, test coverage, style).
4. **ExecuteTasks** runs tasks in parallel with `errgroup`. Each task: allocate a token-budgeted slice → run static tools → run model inference → collect findings.
5. **FilterFindings** applies confidence thresholds, deduplication (dual-hash fingerprinting), rate limits, and escalation rules.
6. **PostComments** publishes findings to GitHub — high-confidence as inline comments, medium-confidence in a summary table, low-confidence suppressed unless escalated.

### What is a "slice"?

A slice is the token-budgeted context window assembled for a single review task. It contains three parts:

| Component | Default budget | Purpose |
|-----------|---------------|---------|
| Diff hunk | 60% | The actual changed lines plus surrounding context |
| Call graph | 25% | Callers and callees — how the function is used |
| Test context | 15% | Tests that exercise the changed function |

The budget is a cap, not a fixed allocation. If a private function has no callers and no tests, the entire budget goes to diff context. Task type shifts the caps (security tasks get more call graph; test coverage tasks get more test context; style tasks skip call graph and tests entirely).

### How does finding deduplication work?

Every finding gets two hashes:

- **Location hash:** `sha256(repo + file_path + symbol + category)` — identifies *where* and *what type* of issue.
- **Content hash:** `sha256(AST subtree of flagged code)` — identifies the *actual code* at that location.

The dedup rule: suppress a finding if the location hash matches a prior finding for the same PR *and* the content hash is unchanged. If the content hash differs (code changed but same pattern), the finding re-surfaces. No TTL — the content hash is the invalidation signal.

## Models and Tools

### What LLMs does Mimir support?

Mimir is model-agnostic by design. The `ModelAdapter` interface accepts any provider:

```go
type ModelAdapter interface {
    Infer(ctx context.Context, task ReviewTask, slice Slice) ([]Finding, error)
    ModelID() string
    MaxTokens() int
}
```

M1 ships with an Anthropic adapter. OpenAI and Gemini adapters are planned for M2. You can implement your own adapter for any model, including self-hosted ones.

### What static analysis tools does it integrate with?

The `StaticToolAdapter` interface supports any tool that can analyze files and return findings:

```go
type StaticToolAdapter interface {
    Run(ctx context.Context, task ReviewTask, files []string) ([]Finding, error)
    ToolName() string
    Languages() []string
}
```

Planned integrations: semgrep and golangci-lint. The interface is intentionally simple — wrapping a new tool is a single struct with three methods.

### Does my code leave my infrastructure?

Mimir runs on your infrastructure. Code diffs are sent to whichever model provider you configure — if that's a cloud API (Anthropic, OpenAI), then yes, diffs leave your network. If you point the `ModelAdapter` at a self-hosted model, they don't. Mimir itself stores PR metadata and findings in your PostgreSQL database and nowhere else.

## Usage

### What are the prerequisites?

- Go 1.23+
- Docker (for PostgreSQL)
- [sqlc](https://docs.sqlc.dev) (for code generation, development only)

### How do I run it locally?

```bash
docker compose up -d          # Start PostgreSQL on port 5433
make build                    # Build the binary
./bin/mimir migrate up        # Apply database migrations
./bin/mimir serve --addr :8080 --workers 4  # Start webhook server + workers
```

See the [README](README.md) for full details on CLI commands and environment variables.

### Can I run the webhook server and workers separately?

Yes. The `serve` command supports split-mode operation:

```bash
./bin/mimir serve --workers=0           # HTTP-only (webhook receiver, no workers)
./bin/mimir serve --http=false          # Worker-only (no HTTP server)
```

This lets you scale workers independently of the webhook receiver.

### Which GitHub events does Mimir handle?

Pull request events with action `opened` or `synchronize` (new push to an existing PR). All other events are ignored.

## Roadmap

### What's in each milestone?

| Milestone | Scope | Timeline |
|-----------|-------|----------|
| **M1** | GitHub adapter, task planner + slice builder, static checks + one model adapter, finding dedup + comment posting, basic scorecard | 4 weeks |
| **M2** | Risk-aware routing, replay/eval harness, better confidence scoring, multi-model adapter | 4–6 weeks |
| **M3** | GitLab adapter, queue-backed service mode, integration cookbook, governance | 6–8 weeks |

### Will Mimir support GitLab?

Yes, in M3. The `ProviderAdapter` interface abstracts the code hosting platform. GitHub is M1; GitLab is the first additional provider.

### Is there a hosted/SaaS version?

No. Mimir is a self-hosted tool. There are no plans for a hosted offering.

## Contributing

### How is the codebase organized?

```
adr/              Accepted Architecture Decision Records (immutable)
scratchpad/       In-progress design notes and open questions
cmd/mimir/        CLI entry point (Cobra)
internal/         Application code (not importable by external packages)
  core/           Domain types — no I/O, no dependencies
  ingest/         GitHub webhook handler
  index/          Semantic repo map (not yet implemented)
  planner/        Task generation (not yet implemented)
  runtime/        Model + tool execution (not yet implemented)
  policy/         Finding filter and dedup (not yet implemented)
  eval/           Metrics and replay (not yet implemented)
  queue/          River job types and worker
  store/          PostgreSQL persistence (sqlc-generated)
pkg/adapter/      Exported plugin interface types
```

### Where are the design decisions documented?

Accepted decisions live in `adr/` and are immutable. In-progress design work lives in `scratchpad/`. The `scratchpad/design-overview.md` file is the best starting point for understanding the full pipeline.

### How do I run the tests?

```bash
make test    # Requires PostgreSQL running (docker compose up -d)
```

Tests hit a real database — there are no mocks for the storage layer.
