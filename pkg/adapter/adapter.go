package adapter

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/mimir/internal/core"
)

// ---------------------------------------------------------------------------
// ProviderAdapter — code hosting platform (GitHub M1, GitLab M2+)
// ---------------------------------------------------------------------------

// ProviderAdapter fetches PR data and posts review results to a code host.
type ProviderAdapter interface {
	// FetchPR returns normalized PR metadata, diff, and file list.
	FetchPR(ctx context.Context, repoFullName string, prNumber int) (*core.PullRequest, error)

	// ListCommits returns commit SHAs for the PR in chronological order.
	ListCommits(ctx context.Context, repoFullName string, prNumber int) ([]string, error)

	// PostComment posts a finding as an inline review comment on a specific diff line.
	// Returns the platform's comment ID for tracking.
	PostComment(ctx context.Context, req PostCommentRequest) (int64, error)

	// PostSummaryComment posts or updates a top-level PR comment (not inline).
	// Returns the platform's comment ID.
	PostSummaryComment(ctx context.Context, repoFullName string, prNumber int, body string) (int64, error)
}

// PostCommentRequest contains the parameters for posting an inline review comment.
type PostCommentRequest struct {
	RepoFullName string `json:"repo_full_name"`
	PRNumber     int    `json:"pr_number"`
	CommitSHA    string `json:"commit_sha"`
	FilePath     string `json:"file_path"`
	Line         int    `json:"line"` // diff line number (GitHub convention)
	Body         string `json:"body"`
}

// ---------------------------------------------------------------------------
// ModelAdapter — LLM provider (Anthropic M1; OpenAI, Gemini M2+)
// ---------------------------------------------------------------------------

// ModelAdapter sends a review task + context slice to a model and returns findings.
type ModelAdapter interface {
	// Infer runs model inference for a single review task.
	// The slice contains the token-budgeted context (diff, call graph, tests).
	Infer(ctx context.Context, task core.ReviewTask, slice core.Slice) ([]core.Finding, error)

	// ModelID returns the identifier string (e.g. "claude-opus-4-6").
	ModelID() string

	// MaxTokens returns the model's context window size in tokens.
	MaxTokens() int
}

// ---------------------------------------------------------------------------
// StaticToolAdapter — static analysis tools (semgrep, golangci-lint, custom)
// ---------------------------------------------------------------------------

// StaticToolAdapter runs a static analysis tool over changed files.
type StaticToolAdapter interface {
	// Run executes the tool and returns findings.
	// files is the subset of PR-changed files relevant to this task.
	Run(ctx context.Context, task core.ReviewTask, files []string) ([]core.Finding, error)

	// ToolName returns the tool identifier (e.g. "semgrep", "golangci-lint").
	ToolName() string

	// Languages returns the file extensions this tool supports (e.g. [".go", ".py"]).
	Languages() []string
}

// ---------------------------------------------------------------------------
// IndexAdapter — semantic repo map (tree-sitter M1, LSP M2)
// ---------------------------------------------------------------------------

// IndexAdapter builds and queries the semantic repo map for a review run.
type IndexAdapter interface {
	// BuildSymbolTable runs the change cone algorithm (ADR-0004): parses changed
	// files, finds importers, extracts references, and builds the per-run symbol table.
	// Called once per pipeline run; the returned SymbolTable is passed to Query calls.
	BuildSymbolTable(ctx context.Context, repoPath, baseSHA, headSHA string) (*core.SymbolTable, error)

	// Query returns context slices for a review task, respecting the token budget.
	Query(ctx context.Context, req IndexRequest) ([]core.Slice, error)

	// IsApproximate returns true if the index is heuristic (not type-resolved).
	// When true, downstream consumers apply a confidence penalty and annotate
	// prompts with an approximation warning (see ADR-0004).
	IsApproximate() bool
}

// IndexRequest specifies what the index should look up and how much room it has.
type IndexRequest struct {
	RepoPath    string           `json:"repo_path"`
	ChangedFile string           `json:"changed_file"`
	Symbol      string           `json:"symbol"`
	Budget      core.TokenBudget `json:"budget"`
}

// ---------------------------------------------------------------------------
// PolicyAdapter — finding gating, triage, and escalation
// ---------------------------------------------------------------------------

// PolicyAdapter gates findings before they are posted to the code host.
// See ADR-0005 for the two-tier posting strategy.
type PolicyAdapter interface {
	// ShouldPost returns true if the individual finding should be posted.
	// Checks confidence tier, severity, dedup state, and rate limits.
	ShouldPost(ctx context.Context, f core.Finding) bool

	// ShouldEscalate returns true if the finding should bypass normal
	// suppression rules (e.g. low-confidence but security/critical).
	ShouldEscalate(ctx context.Context, f core.Finding) bool

	// MaxFindingsPerPR returns the cap on inline comments per PR.
	// Default: 7. Overflow findings move to the summary comment.
	MaxFindingsPerPR() int

	// Triage partitions findings into posting tiers: inline (diff comments),
	// summary (table in the summary comment), suppress (not posted).
	// Enforces the inline cap, escalation rules, and severity priority.
	Triage(ctx context.Context, findings []core.Finding) (inline, summary, suppress []core.Finding)
}

// ---------------------------------------------------------------------------
// StoreAdapter — persistence (PostgreSQL only, ADR-0002)
// ---------------------------------------------------------------------------

// TxFunc is a callback that receives a transaction-scoped StoreAdapter and
// the raw pgx.Tx for components (e.g. River) that need direct tx access.
type TxFunc func(txStore StoreAdapter, tx pgx.Tx) error

// StoreAdapter is the persistence boundary for all domain objects.
type StoreAdapter interface {
	// --- Transactions ---

	// WithTx runs fn inside a database transaction. The txStore argument
	// passed to fn is bound to the transaction. If fn returns an error,
	// the transaction is rolled back; otherwise it is committed.
	// The raw pgx.Tx is exposed for components like River that need it.
	WithTx(ctx context.Context, fn TxFunc) error

	// --- Pull Requests ---

	// UpsertPullRequest creates or updates a PR record. Upsert key is
	// (github_pr_id, head_sha).
	UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error

	// GetPullRequest retrieves a PR by its internal UUID.
	GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error)

	// SoftDeletePullRequest sets deleted_at on a PR record.
	SoftDeletePullRequest(ctx context.Context, id uuid.UUID) error

	// --- Pipeline Runs ---

	// CreatePipelineRun inserts a new pipeline run record.
	CreatePipelineRun(ctx context.Context, run *core.PipelineRun) error

	// CompletePipelineRun marks a pipeline run as completed or failed,
	// recording final task/finding counts and optional error.
	CompletePipelineRun(ctx context.Context, id uuid.UUID, status core.PipelineRunStatus, taskCount, findingCount int, errMsg *string) error

	// GetPipelineRun retrieves a pipeline run by its internal UUID.
	GetPipelineRun(ctx context.Context, id uuid.UUID) (*core.PipelineRun, error)

	// ListPipelineRunsForPR returns all pipeline runs for a given pull request.
	ListPipelineRunsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.PipelineRun, error)

	// ReconcileStalePipelineRuns marks any runs still in 'running' status
	// as 'failed'. Called at startup to clean up after crashes.
	ReconcileStalePipelineRuns(ctx context.Context) (int64, error)

	// --- Review Tasks ---

	// CreateReviewTask inserts a new review task.
	CreateReviewTask(ctx context.Context, task *core.ReviewTask) error

	// UpdateReviewTaskStatus transitions a task's status and optionally
	// records an error message (for failed tasks).
	UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status core.TaskStatus, errMsg *string) error

	// ListPendingReviewTasks returns tasks with status = 'pending',
	// ordered by risk_score descending.
	ListPendingReviewTasks(ctx context.Context) ([]core.ReviewTask, error)

	// --- Findings ---

	// CreateFinding inserts a new finding. Every finding is preserved
	// (no upsert — append-only by design).
	CreateFinding(ctx context.Context, f *core.Finding) error

	// ListFindingsForPR returns all findings for a given pull request,
	// ordered by confidence descending, severity priority ascending.
	ListFindingsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error)

	// FindPriorFinding looks up a previous finding with the same location hash
	// for the given repo. Used by policy for cross-run dedup.
	FindPriorFinding(ctx context.Context, locationHash, repoFullName string) (*core.Finding, error)

	// ListUnaddressedFindings returns posted findings that have not been
	// addressed in a subsequent commit. Used by addressed-in-next-commit detection.
	ListUnaddressedFindings(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error)

	// ListUnpostedFindings returns findings that should have been posted
	// but lack a posted_at timestamp. Used by the posting retry job.
	ListUnpostedFindings(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error)

	// MarkFindingPosted records the platform comment ID and timestamp
	// after a finding is posted as a review comment.
	MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error

	// MarkFindingAddressed sets addressed_in_next_commit = true.
	MarkFindingAddressed(ctx context.Context, id uuid.UUID) error

	// --- Dismissed Fingerprints ---

	// IsFingerprintDismissed checks whether a location hash has been
	// permanently dismissed for the given repository.
	IsFingerprintDismissed(ctx context.Context, locationHash, repoFullName string) (bool, error)

	// DismissFingerprint records a permanent dismissal for a location hash
	// in the given repository. Upserts to avoid duplicate key errors.
	DismissFingerprint(ctx context.Context, fingerprint, repoFullName, dismissedBy, reason string) error

	// --- Finding Events ---

	// CreateFindingEvent inserts an append-only lifecycle event for a finding.
	CreateFindingEvent(ctx context.Context, event *core.FindingEvent) error

	// ListEventsForFinding returns all lifecycle events for a given finding,
	// ordered by created_at ascending.
	ListEventsForFinding(ctx context.Context, findingID uuid.UUID) ([]core.FindingEvent, error)
}
