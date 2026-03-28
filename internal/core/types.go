package core

import (
	"time"

	"github.com/google/uuid"
)

// --- Enum types ---

// Category classifies the kind of review a finding addresses.
type Category string

const (
	CategorySecurity     Category = "security"
	CategoryLogic        Category = "logic"
	CategoryTestCoverage Category = "test_coverage"
	CategoryStyle        Category = "style"
	CategoryPerformance  Category = "performance"
)

// ConfidenceTier is the model's self-assessed confidence bucket.
// Tier assignment is model output, not derived from ConfidenceScore alone.
type ConfidenceTier string

const (
	ConfidenceHigh   ConfidenceTier = "high"
	ConfidenceMedium ConfidenceTier = "medium"
	ConfidenceLow    ConfidenceTier = "low"
)

// Severity indicates the impact level of a finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// PRState tracks the lifecycle of a pull request.
type PRState string

const (
	PRStateOpen   PRState = "open"
	PRStateClosed PRState = "closed"
	PRStateMerged PRState = "merged"
)

// TaskStatus tracks the execution lifecycle of a review task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
)

// TaskType is the review lens applied to a changed symbol.
type TaskType string

const (
	TaskTypeSecurity     TaskType = "security"
	TaskTypeLogic        TaskType = "logic"
	TaskTypeTestCoverage TaskType = "test_coverage"
	TaskTypeStyle        TaskType = "style"
)

// RiskScore is the planner's assessment of how much review attention a
// symbol deserves. Higher values mean higher priority in task ordering.
type RiskScore float64

// --- Domain structs ---

// PullRequest is the normalized representation of a PR from any code host.
type PullRequest struct {
	ID           uuid.UUID      `json:"id"`
	GitHubPRID   int64          `json:"github_pr_id"`
	RepoFullName string         `json:"repo_full_name"`
	PRNumber     int            `json:"pr_number"`
	HeadSHA      string         `json:"head_sha"`
	BaseSHA      string         `json:"base_sha"`
	Author       string         `json:"author"`
	State        PRState        `json:"state"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// ReviewTask is a single unit of review work: one changed symbol examined
// through one lens (security, logic, test_coverage, style).
type ReviewTask struct {
	ID            uuid.UUID  `json:"id"`
	PullRequestID uuid.UUID  `json:"pull_request_id"`
	TaskType      TaskType   `json:"task_type"`
	FilePath      string     `json:"file_path"`
	Symbol        string     `json:"symbol,omitempty"`
	RiskScore     RiskScore  `json:"risk_score"`
	Status        TaskStatus `json:"status"`
	Error         *string    `json:"error,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Finding is a single review observation produced by a model or static tool.
type Finding struct {
	ID            uuid.UUID `json:"id"`
	ReviewTaskID  uuid.UUID `json:"review_task_id"`
	PullRequestID uuid.UUID `json:"pull_request_id"`

	// Location within the changed file.
	FilePath  string `json:"file_path"`
	StartLine *int   `json:"start_line,omitempty"` // nil = file-level finding
	EndLine   *int   `json:"end_line,omitempty"`   // nil = single-line or file-level
	Symbol    string `json:"symbol,omitempty"`

	// Classification.
	Category        Category       `json:"category"`
	ConfidenceTier  ConfidenceTier `json:"confidence_tier"`
	ConfidenceScore float64        `json:"confidence_score"` // 0.0–1.0
	Severity        Severity       `json:"severity"`

	// Content shown to the reviewer.
	Title      string `json:"title"`
	Body       string `json:"body"`
	Suggestion string `json:"suggestion,omitempty"` // optional suggested fix

	// Fingerprinting for dedup (ADR-0002).
	// LocationHash = sha256(repo + file_path + symbol + category). No LLM output.
	// ContentHash  = sha256(AST subtree of flagged code). Empty when index unavailable.
	LocationHash string `json:"location_hash"`
	ContentHash  string `json:"content_hash,omitempty"`

	// Lifecycle.
	PostedAt              *time.Time `json:"posted_at,omitempty"`
	GitHubCommentID       *int64     `json:"github_comment_id,omitempty"`
	AddressedInNextCommit bool       `json:"addressed_in_next_commit"`
	DismissedAt           *time.Time `json:"dismissed_at,omitempty"`
	DismissedBy           string     `json:"dismissed_by,omitempty"`

	// Provenance — which model produced this finding, at what cost.
	ModelID          string         `json:"model_id"`
	PromptTokens     *int           `json:"prompt_tokens,omitempty"`
	CompletionTokens *int           `json:"completion_tokens,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SliceSection is one component of the context window sent to the model.
type SliceSection struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// Slice is the token-budgeted context window assembled for a single ReviewTask.
// Constructed by internal/runtime, consumed by ModelAdapter.Infer.
type Slice struct {
	DiffHunk    SliceSection `json:"diff_hunk"`
	CallGraph   SliceSection `json:"call_graph"`
	TestContext SliceSection `json:"test_context"`
}

// TokenBudget specifies the maximum tokens allowed per slice section.
// These are caps, not allocations — unused budget in call graph or tests
// rolls over to the diff hunk at fill time (see slice-budgeting.md).
type TokenBudget struct {
	DiffHunk  int `json:"diff_hunk"`
	CallGraph int `json:"call_graph"`
	Tests     int `json:"tests"`
}
