package adapter

import (
	"context"

	"github.com/google/uuid"

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

	// PostSummaryComment posts a top-level PR comment (not inline).
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

// StoreAdapter is the persistence boundary for all domain objects.
type StoreAdapter interface {
	// UpsertPullRequest creates or updates a PR record. Upsert key is
	// (github_pr_id, head_sha).
	UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error

	// GetPullRequest retrieves a PR by its internal UUID.
	GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error)

	// CreateReviewTask inserts a new review task.
	CreateReviewTask(ctx context.Context, task *core.ReviewTask) error

	// UpdateReviewTaskStatus transitions a task's status and optionally
	// records an error message (for failed tasks).
	UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status core.TaskStatus, errMsg *string) error

	// ListPendingReviewTasks returns tasks with status = 'pending'.
	ListPendingReviewTasks(ctx context.Context) ([]core.ReviewTask, error)

	// CreateFinding inserts a new finding.
	CreateFinding(ctx context.Context, f *core.Finding) error

	// ListFindingsForPR returns all findings for a given pull request.
	ListFindingsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error)

	// MarkFindingPosted records the platform comment ID and timestamp
	// after a finding is posted as a review comment.
	MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error

	// MarkFindingAddressed sets addressed_in_next_commit = true.
	MarkFindingAddressed(ctx context.Context, id uuid.UUID) error

	// IsFingerprintDismissed checks whether a location hash has been
	// permanently dismissed for the given repository.
	IsFingerprintDismissed(ctx context.Context, locationHash, repoFullName string) (bool, error)
}
