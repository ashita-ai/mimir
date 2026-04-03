# Plugin Interfaces (Draft)

> **Status:** Draft — not yet implemented in code. These are design sketches.
> Final signatures will be locked when `pkg/adapter` is implemented.

---

## Design Principles

1. **All adapters accept a `context.Context` first.** Cancellation propagates through the pipeline.
2. **No adapter returns raw errors with I/O detail.** Wrap errors with `fmt.Errorf("...: %w", err)` at the boundary.
3. **Adapters are stateless where possible.** State (DB connections, HTTP clients) is injected at construction, not stored mid-request.
4. **Interfaces are small.** Each interface should have ≤ 5 methods. Split if larger.

---

## `ProviderAdapter`

Abstracts the code hosting platform (GitHub M1, GitLab M2+).

```go
// ProviderAdapter fetches PR data and posts review results.
type ProviderAdapter interface {
    // FetchPR returns normalized PR metadata, diff, and file list.
    FetchPR(ctx context.Context, repoFullName string, prNumber int) (*core.PullRequest, error)

    // ListCommits returns commit SHAs for the PR in chronological order.
    ListCommits(ctx context.Context, repoFullName string, prNumber int) ([]string, error)

    // PostComment posts a finding as an inline review comment on a specific diff line.
    // Returns the platform's comment ID for tracking.
    PostComment(ctx context.Context, req PostCommentRequest) (int64, error)

    // PostSummaryComment posts a top-level PR comment (not inline).
    PostSummaryComment(ctx context.Context, repoFullName string, prNumber int, body string) (int64, error)
}

type PostCommentRequest struct {
    RepoFullName string
    PRNumber     int
    CommitSHA    string
    FilePath     string
    Line         int    // diff line number (GitHub convention)
    Body         string
}
```

---

## `ModelAdapter`

Abstracts LLM provider (Anthropic M1; OpenAI, Gemini M2+).

```go
// ModelAdapter sends a review task + context slice to a model and returns findings.
type ModelAdapter interface {
    // Infer runs model inference for a single review task.
    // slice contains the token-budgeted context (diff, call graph, tests).
    // Returns structured findings from the model's response.
    Infer(ctx context.Context, task core.ReviewTask, slice core.Slice) ([]core.Finding, error)

    // ModelID returns the model identifier string (e.g. "claude-opus-4-6").
    ModelID() string

    // MaxTokens returns the model's context window limit.
    MaxTokens() int
}
```

---

## `StaticToolAdapter`

Abstracts static analysis tools (semgrep, golangci-lint, custom).

```go
// StaticToolAdapter runs a static analysis tool over a set of files.
type StaticToolAdapter interface {
    // Run executes the tool and returns findings.
    // files is the subset of PR-changed files relevant to this task.
    Run(ctx context.Context, task core.ReviewTask, files []string) ([]core.Finding, error)

    // ToolName returns the tool identifier (e.g. "semgrep", "golangci-lint").
    ToolName() string

    // Languages returns the file extensions this tool supports (e.g. [".go", ".py"]).
    Languages() []string
}
```

---

## `IndexAdapter`

Abstracts the semantic repo map backend (tree-sitter M1, LSP M2).

```go
// IndexAdapter builds and queries the semantic repo index.
type IndexAdapter interface {
    // Query returns context slices for a review task.
    // The implementation decides how to allocate the token budget across slice types.
    Query(ctx context.Context, req IndexRequest) ([]core.Slice, error)

    // IsApproximate returns true if the index is heuristic (not type-resolved).
    // When true, findings derived from this index should be labeled "approximate context".
    IsApproximate() bool
}

type IndexRequest struct {
    RepoPath    string
    ChangedFile string
    Symbol      string      // function/type being reviewed
    Budget      TokenBudget // how many tokens are available, split by type
}

type TokenBudget struct {
    DiffHunk  int // tokens for the raw diff context
    CallGraph int // tokens for callers/callees
    Tests     int // tokens for related test functions
}
```

---

## `PolicyAdapter`

Controls what gets posted and when. See `adr/0005-service-architecture.md` review notes for the two-tier posting strategy.

```go
// PolicyAdapter gates findings before they are posted to the provider.
type PolicyAdapter interface {
    // ShouldPost returns true if the finding should be posted as a comment.
    // Implementations check confidence tier, severity, dedup state, and rate limits.
    // Retained for simple policy implementations that don't need batch logic.
    ShouldPost(ctx context.Context, f core.Finding) bool

    // ShouldEscalate returns true if the finding should bypass normal suppression rules.
    // Escalated findings are always posted, regardless of confidence tier or dedup.
    ShouldEscalate(ctx context.Context, f core.Finding) bool

    // MaxFindingsPerPR returns the cap on inline findings per PR.
    // Default: 7. Overflow findings move to summary comment.
    MaxFindingsPerPR() int

    // Triage partitions findings into posting tiers.
    // Returns: inline (posted as diff comments), summary (included in summary
    // table only), suppress (not posted at all).
    // Enforces the inline cap, escalation rules, and prioritizes by severity.
    // Implementations may delegate to ShouldPost/ShouldEscalate internally.
    Triage(ctx context.Context, findings []core.Finding) (inline, summary, suppress []core.Finding)
}
```

---

## `StoreAdapter`

Persists and retrieves domain objects. Backed by PostgreSQL only (ADR-0002).

```go
// StoreAdapter is the persistence boundary for all domain objects.
// All implementations use PostgreSQL (ADR-0002).
type StoreAdapter interface {
    // PullRequests
    UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error
    GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error)

    // ReviewTasks
    CreateReviewTask(ctx context.Context, task *core.ReviewTask) error
    UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error
    ListPendingReviewTasks(ctx context.Context) ([]core.ReviewTask, error)

    // Findings
    CreateFinding(ctx context.Context, f *core.Finding) error
    ListFindingsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error)
    MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error
    MarkFindingAddressed(ctx context.Context, id uuid.UUID) error
    IsFingerprintDismissed(ctx context.Context, fingerprint, repoFullName string) (bool, error)
}
```

---

## Notes

- `pkg/adapter` exports the interface types. `internal/` contains implementations.
- The `internal/store` package implements `StoreAdapter` using sqlc-generated queries.
- M1 ships: `GitHubProvider`, `AnthropicModel`, `TreeSitterIndex`, `DefaultPolicy`, `PostgresStore`.
- Each adapter should have a constructor that accepts its dependencies explicitly — no global state, no `init()` side effects.
