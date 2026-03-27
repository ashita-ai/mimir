# Spec 01: Core Domain Types

> **Status:** M1 Build Spec
> **Date:** 2026-03-27
> **Package:** `internal/core`

---

## Constraints

- No I/O. No external dependencies. No database imports, no HTTP clients.
- Pure data types, enums, validation functions, and value objects.
- Every other package depends on `core`. Changes here ripple everywhere.

---

## Types

### PullRequest

```go
type PullRequest struct {
    ID            uuid.UUID
    GitHubPRID    int64
    RepoFullName  string          // "owner/repo"
    PRNumber      int
    HeadSHA       string
    BaseSHA       string
    Author        string          // GitHub username
    State         PRState         // open | closed | merged
    Diff          string          // full unified diff
    ChangedFiles  []string        // file paths from diff
    Metadata      json.RawMessage // extensible JSONB envelope
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

type PRState string

const (
    PRStateOpen   PRState = "open"
    PRStateClosed PRState = "closed"
    PRStateMerged PRState = "merged"
)
```

### ReviewTask

```go
type ReviewTask struct {
    ID              uuid.UUID
    PullRequestID   uuid.UUID
    TaskType        TaskType
    FilePath        string
    Symbol          string          // function/type name; empty for file-level tasks
    RiskScore       RiskScore
    ModelID         string          // resolved model for this task (e.g. "claude-opus-4-6")
    Status          TaskStatus
    Error           *string         // populated on failure
    StartedAt       *time.Time
    CompletedAt     *time.Time
    CreatedAt       time.Time
}

type TaskType string

const (
    TaskTypeSecurity     TaskType = "security"
    TaskTypeLogic        TaskType = "logic"
    TaskTypeTestCoverage TaskType = "test_coverage"
    TaskTypeStyle        TaskType = "style"
)

type TaskStatus string

const (
    TaskStatusPending   TaskStatus = "pending"
    TaskStatusRunning   TaskStatus = "running"
    TaskStatusCompleted TaskStatus = "completed"
    TaskStatusFailed    TaskStatus = "failed"
)

type RiskScore float64

// HighRisk is the threshold above which a symbol always gets reviewed.
const HighRiskThreshold RiskScore = 0.7

// LowRisk is the threshold below which a symbol is skipped.
const LowRiskThreshold RiskScore = 0.3
```

### Finding

```go
type Finding struct {
    ID              uuid.UUID
    ReviewTaskID    uuid.UUID
    PullRequestID   uuid.UUID

    // Location
    FilePath        string
    StartLine       *int            // nil for file-level findings
    EndLine         *int
    Symbol          string          // function/type name if applicable

    // Classification
    Category        FindingCategory
    ConfidenceTier  ConfidenceTier
    ConfidenceScore float64         // 0.0–1.0, continuous
    Severity        Severity

    // Content
    Title           string
    Body            string
    Suggestion      string          // optional: suggested fix

    // Fingerprinting
    LocationHash    string          // sha256(repo + file_path + symbol + category)
    ContentHash     *string         // sha256(AST subtree); nil if index unavailable

    // Lifecycle
    PostedAt        *time.Time
    GitHubCommentID *int64
    AddressedStatus AddressedStatus // unaddressed | likely_addressed | confirmed
    DismissedAt     *time.Time
    DismissedBy     *string         // GitHub username

    // Provenance
    ModelID         string
    PromptTokens    *int
    CompletionTokens *int
    Metadata        json.RawMessage

    CreatedAt       time.Time
    UpdatedAt       time.Time
}

type FindingCategory string

const (
    CategorySecurity     FindingCategory = "security"
    CategoryLogic        FindingCategory = "logic"
    CategoryTestCoverage FindingCategory = "test_coverage"
    CategoryStyle        FindingCategory = "style"
    CategoryPerformance  FindingCategory = "performance"
)

type ConfidenceTier string

const (
    ConfidenceHigh   ConfidenceTier = "high"    // 0.80–1.00
    ConfidenceMedium ConfidenceTier = "medium"  // 0.50–0.79
    ConfidenceLow    ConfidenceTier = "low"     // 0.00–0.49
)

type Severity string

const (
    SeverityCritical Severity = "critical"
    SeverityHigh     Severity = "high"
    SeverityMedium   Severity = "medium"
    SeverityLow      Severity = "low"
    SeverityInfo     Severity = "info"
)

type AddressedStatus string

const (
    AddressedUnaddressed     AddressedStatus = "unaddressed"
    AddressedLikelyAddressed AddressedStatus = "likely_addressed"
    AddressedConfirmed       AddressedStatus = "confirmed"
)
```

### Slice

```go
// Slice is the token-budgeted context window for a single ReviewTask.
type Slice struct {
    DiffHunk     string // raw diff lines + surrounding context
    CallGraph    string // callers/callees from IndexAdapter
    TestContext  string // test functions exercising the changed symbol
    Truncated    bool   // true if any section was truncated
    Approximate  bool   // true if call graph / test context came from heuristic index
}
```

### SymbolTable

```go
// SymbolTable is built once per pipeline run and discarded.
type SymbolTable struct {
    FileSymbols map[string][]Symbol     // file path → symbols defined
    SymbolRefs  map[string][]Reference  // symbol name → reference locations (changed symbols only)
    TestFiles   map[string][]string     // source file → test file(s)
    ImportGraph map[string][]string     // file path → imported package paths
}

type Symbol struct {
    Name      string
    Kind      SymbolKind // func | method | type | interface
    FilePath  string
    StartLine int
    EndLine   int
    Package   string     // resolved package path
    Exported  bool       // starts with uppercase (Go), exported keyword, etc.
}

type SymbolKind string

const (
    SymbolFunc      SymbolKind = "func"
    SymbolMethod    SymbolKind = "method"
    SymbolType      SymbolKind = "type"
    SymbolInterface SymbolKind = "interface"
)

type Reference struct {
    FilePath string
    Line     int
    InFunc   string // enclosing function name, if any
}
```

### TokenBudget

```go
type TokenBudget struct {
    DiffHunk  int // max tokens for diff context
    CallGraph int // max tokens for caller/callee context
    Tests     int // max tokens for test context
    Total     int // hard cap across all sections
}
```

---

## Fingerprint Functions

```go
// LocationFingerprint produces a deterministic hash for dedup across runs.
// No LLM output in the hash — fully deterministic.
func LocationFingerprint(repoFullName, filePath, symbol string, category FindingCategory) string {
    h := sha256.New()
    io.WriteString(h, repoFullName)
    io.WriteString(h, "\x00")
    io.WriteString(h, filePath)
    io.WriteString(h, "\x00")
    io.WriteString(h, symbol)
    io.WriteString(h, "\x00")
    io.WriteString(h, string(category))
    return hex.EncodeToString(h.Sum(nil))
}

// ContentFingerprint produces a hash of the AST subtree for the flagged code region.
// Returns empty string if the AST is unavailable.
func ContentFingerprint(astSubtree []byte) string {
    if len(astSubtree) == 0 {
        return ""
    }
    h := sha256.Sum256(astSubtree)
    return hex.EncodeToString(h[:])
}
```

---

## Validation

```go
// ValidateConfidenceTier checks that score matches tier.
func ValidateConfidenceTier(tier ConfidenceTier, score float64) error {
    switch tier {
    case ConfidenceHigh:
        if score < 0.80 {
            return fmt.Errorf("high confidence tier requires score >= 0.80, got %.2f", score)
        }
    case ConfidenceMedium:
        if score < 0.50 || score >= 0.80 {
            return fmt.Errorf("medium confidence tier requires 0.50 <= score < 0.80, got %.2f", score)
        }
    case ConfidenceLow:
        if score >= 0.50 {
            return fmt.Errorf("low confidence tier requires score < 0.50, got %.2f", score)
        }
    default:
        return fmt.Errorf("unknown confidence tier: %s", tier)
    }
    return nil
}
```

Policy may **downgrade** a tier (e.g., apply 0.85× penalty pushing a high-confidence finding to medium). Policy must never **upgrade** a tier. `ValidateConfidenceTier` is called after any policy adjustment to enforce consistency.
