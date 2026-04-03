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

// PipelineRunStatus tracks the execution lifecycle of a pipeline run.
type PipelineRunStatus string

const (
	PipelineRunStatusRunning   PipelineRunStatus = "running"
	PipelineRunStatusCompleted PipelineRunStatus = "completed"
	PipelineRunStatusFailed    PipelineRunStatus = "failed"
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
// Diff and ChangedFiles are transient fields populated by ProviderAdapter.FetchPR
// and passed through the pipeline — they are not persisted in the database.
type PullRequest struct {
	ID           uuid.UUID      `json:"id"`
	ExternalPRID   int64          `json:"external_pr_id"`
	RepoFullName string         `json:"repo_full_name"`
	PRNumber     int            `json:"pr_number"`
	HeadSHA      string         `json:"head_sha"`
	BaseSHA      string         `json:"base_sha"`
	Author       string         `json:"author"`
	State        PRState        `json:"state"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	DeletedAt    *time.Time     `json:"deleted_at,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`

	// Transient fields — populated by FetchPR, not persisted.
	Diff         string   `json:"diff,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
}

// PipelineRun is the primary audit record for a single pipeline execution.
// It anchors all review tasks and findings to a specific run, captures the
// config/prompt version at execution time, and enables stale run reconciliation.
type PipelineRun struct {
	ID            uuid.UUID         `json:"id"`
	PullRequestID uuid.UUID         `json:"pull_request_id"`
	HeadSHA       string            `json:"head_sha"`
	Status        PipelineRunStatus `json:"status"`
	PromptVersion string            `json:"prompt_version"`
	ConfigHash    string            `json:"config_hash"`
	TaskCount     int               `json:"task_count"`
	FindingCount  int               `json:"finding_count"`
	Error         *string           `json:"error,omitempty"`
	Metadata      map[string]any    `json:"metadata,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// ReviewTask is a single unit of review work: one changed symbol examined
// through one lens (security, logic, test_coverage, style).
type ReviewTask struct {
	ID            uuid.UUID  `json:"id"`
	PullRequestID uuid.UUID  `json:"pull_request_id"`
	PipelineRunID uuid.UUID  `json:"pipeline_run_id"`
	TaskType      TaskType   `json:"task_type"`
	FilePath      string     `json:"file_path"`
	Symbol        string     `json:"symbol,omitempty"`
	RiskScore     RiskScore  `json:"risk_score"`
	ModelID       string     `json:"model_id"`
	DiffHunk      string     `json:"diff_hunk,omitempty"`
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
	PipelineRunID uuid.UUID `json:"pipeline_run_id"`

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
	HeadSHA      string `json:"head_sha"`

	// Lifecycle.
	PostedAt              *time.Time `json:"posted_at,omitempty"`
	ExternalCommentID       *int64     `json:"external_comment_id,omitempty"`
	AddressedInNextCommit bool       `json:"addressed_in_next_commit"`
	SuppressionReason     string     `json:"suppression_reason,omitempty"`
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

// FindingEvent is an append-only audit log entry for a finding's lifecycle.
// Records reactions, status changes, dismissals — the raw signal for eval.
type FindingEvent struct {
	ID        uuid.UUID      `json:"id"`
	FindingID uuid.UUID      `json:"finding_id"`
	EventType string         `json:"event_type"`
	Actor     string         `json:"actor,omitempty"`
	OldValue  string         `json:"old_value,omitempty"`
	NewValue  string         `json:"new_value,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// SymbolTable is built once per pipeline run and discarded. It holds the
// semantic index output: symbols defined in each file, cross-file references
// for changed symbols, test file mappings, and import graph for disambiguation.
// See ADR-0004 for the change cone algorithm that populates this.
type SymbolTable struct {
	// FileSymbols maps file path to symbols defined in that file.
	FileSymbols map[string][]Symbol `json:"file_symbols"`

	// SymbolRefs maps symbol name to locations where it's referenced.
	// Populated only for changed symbols (not the entire repo).
	SymbolRefs map[string][]Reference `json:"symbol_refs"`

	// TestFiles maps source file to corresponding test file(s).
	// Heuristic: foo.go -> foo_test.go in the same package.
	TestFiles map[string][]string `json:"test_files"`

	// ImportGraph maps file path to imported package paths.
	// Used for the two-pass disambiguation filter (ADR-0004).
	ImportGraph map[string][]string `json:"import_graph"`
}

// Symbol represents a named code entity extracted by tree-sitter.
type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "func" | "method" | "type" | "interface"
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// Reference is a location where a symbol is used.
type Reference struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	InFunc   string `json:"in_func,omitempty"` // enclosing function name, if any
}

// APIError is a structured error from a provider or model adapter call.
// It carries enough information for the retry logic to classify the failure.
type APIError struct {
	StatusCode int            `json:"status_code"`
	Message    string         `json:"message"`
	Provider   string         `json:"provider"`
	Retryable  bool           `json:"retryable"`
	RetryAfter *time.Duration `json:"retry_after,omitempty"` // from Retry-After header, if present
}

func (e *APIError) Error() string {
	return e.Provider + ": " + e.Message
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
