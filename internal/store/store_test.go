package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ashita-ai/mimir/internal/core"
	"github.com/ashita-ai/mimir/internal/store"
	"github.com/ashita-ai/mimir/pkg/adapter"
)

// Compile-time check: *store.Store satisfies adapter.StoreAdapter.
var _ adapter.StoreAdapter = (*store.Store)(nil)

// testDatabaseURL returns the database URL for integration tests.
// Defaults to the docker-compose local dev database.
// Set MIMIR_TEST_DATABASE_URL to override.
func testDatabaseURL() string {
	if v := os.Getenv("MIMIR_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://mimir:mimir@localhost:5433/mimir?sslmode=disable"
}

// setupTestDB creates a pgxpool, runs migrations, and returns the pool
// and a cleanup function that truncates all tables.
func setupTestDB(t *testing.T) (*pgxpool.Pool, *store.Store) {
	t.Helper()

	dbURL := testDatabaseURL()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("skipping integration test: cannot connect to database: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skipping integration test: database not reachable: %v", err)
	}

	// Run river migrations.
	riverMigrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	require.NoError(t, err)
	_, err = riverMigrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	require.NoError(t, err)

	// Run app migrations via goose.
	sqlDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer sqlDB.Close()

	migrationFS, err := fs.Sub(store.Migrations, "migrations")
	require.NoError(t, err)

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationFS)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	// Truncate tables before each test.
	tables := []string{"findings", "review_tasks", "pull_requests", "dismissed_fingerprints"}
	for _, table := range tables {
		_, err := pool.Exec(ctx, fmt.Sprintf("TRUNCATE %s CASCADE", table))
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		pool.Close()
	})

	return pool, store.New(pool)
}

// ---------------------------------------------------------------------------
// PullRequest tests
// ---------------------------------------------------------------------------

func TestUpsertAndGetPullRequest(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := &core.PullRequest{
		GitHubPRID:   12345,
		RepoFullName: "ashita-ai/mimir",
		PRNumber:     42,
		HeadSHA:      "abc123",
		BaseSHA:      "def456",
		Author:       "testuser",
		State:        core.PRStateOpen,
		Metadata:     map[string]any{"source": "test"},
	}

	// Insert.
	err := st.UpsertPullRequest(ctx, pr)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, pr.ID)
	assert.False(t, pr.CreatedAt.IsZero())

	// Get.
	got, err := st.GetPullRequest(ctx, pr.ID)
	require.NoError(t, err)
	assert.Equal(t, pr.ID, got.ID)
	assert.Equal(t, pr.RepoFullName, got.RepoFullName)
	assert.Equal(t, pr.PRNumber, got.PRNumber)
	assert.Equal(t, pr.HeadSHA, got.HeadSHA)
	assert.Equal(t, pr.Author, got.Author)
	assert.Equal(t, "test", got.Metadata["source"])

	// Upsert same (github_pr_id, head_sha) — should update, not insert.
	pr.State = core.PRStateMerged
	err = st.UpsertPullRequest(ctx, pr)
	require.NoError(t, err)

	got2, err := st.GetPullRequest(ctx, pr.ID)
	require.NoError(t, err)
	assert.Equal(t, core.PRStateMerged, got2.State)
}

func TestGetPullRequest_NotFound(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	_, err := st.GetPullRequest(ctx, uuid.New())
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// ReviewTask tests
// ---------------------------------------------------------------------------

func TestCreateAndListReviewTasks(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)

	task := &core.ReviewTask{
		PullRequestID: pr.ID,
		TaskType:      core.TaskTypeSecurity,
		FilePath:      "internal/auth/token.go",
		Symbol:        "ValidateToken",
		RiskScore:     0.85,
	}

	err := st.CreateReviewTask(ctx, task)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, task.ID)
	assert.Equal(t, core.TaskStatusPending, task.Status)

	// List pending — should include our task.
	pending, err := st.ListPendingReviewTasks(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, task.ID, pending[0].ID)
	assert.Equal(t, "ValidateToken", pending[0].Symbol)
}

func TestUpdateReviewTaskStatus(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	task := insertTestTask(t, st, pr.ID)

	// Transition to running — should set started_at.
	err := st.UpdateReviewTaskStatus(ctx, task.ID, core.TaskStatusRunning, nil)
	require.NoError(t, err)

	// Should no longer appear in pending.
	pending, err := st.ListPendingReviewTasks(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending)

	// Transition to failed — should set completed_at and error.
	errMsg := "model timeout"
	err = st.UpdateReviewTaskStatus(ctx, task.ID, core.TaskStatusFailed, &errMsg)
	require.NoError(t, err)
}

func TestUpdateReviewTaskStatus_NotFound(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	err := st.UpdateReviewTaskStatus(ctx, uuid.New(), core.TaskStatusRunning, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Finding tests
// ---------------------------------------------------------------------------

func TestCreateAndListFindings(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	task := insertTestTask(t, st, pr.ID)

	line := 42
	f := &core.Finding{
		ReviewTaskID:    task.ID,
		PullRequestID:   pr.ID,
		FilePath:        "internal/auth/token.go",
		StartLine:       &line,
		Symbol:          "ValidateToken",
		Category:        core.CategorySecurity,
		ConfidenceTier:  core.ConfidenceHigh,
		ConfidenceScore: 0.92,
		Severity:        core.SeverityHigh,
		Title:           "Unchecked JWT expiry",
		Body:            "The token validation does not verify the exp claim.",
		LocationHash:    core.ComputeLocationHash("ashita-ai/mimir", "internal/auth/token.go", "ValidateToken", core.CategorySecurity),
		ModelID:         "claude-opus-4-6",
		Metadata:        map[string]any{},
	}

	err := st.CreateFinding(ctx, f)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, f.ID)

	// List findings for PR.
	findings, err := st.ListFindingsForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, f.Title, findings[0].Title)
	assert.Equal(t, 0.92, findings[0].ConfidenceScore)
	assert.Equal(t, core.SeverityHigh, findings[0].Severity)
	assert.Equal(t, 42, *findings[0].StartLine)
}

func TestMarkFindingPosted(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	task := insertTestTask(t, st, pr.ID)
	f := insertTestFinding(t, st, pr.ID, task.ID)

	err := st.MarkFindingPosted(ctx, f.ID, 999888)
	require.NoError(t, err)

	findings, err := st.ListFindingsForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.NotNil(t, findings[0].PostedAt)
	assert.NotNil(t, findings[0].GitHubCommentID)
	assert.Equal(t, int64(999888), *findings[0].GitHubCommentID)
}

func TestMarkFindingAddressed(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	task := insertTestTask(t, st, pr.ID)
	f := insertTestFinding(t, st, pr.ID, task.ID)

	err := st.MarkFindingAddressed(ctx, f.ID)
	require.NoError(t, err)

	findings, err := st.ListFindingsForPR(ctx, pr.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.True(t, findings[0].AddressedInNextCommit)
}

func TestFindingLocationHashUniqueness(t *testing.T) {
	_, st := setupTestDB(t)
	ctx := context.Background()

	pr := insertTestPR(t, st)
	task := insertTestTask(t, st, pr.ID)

	hash := core.ComputeLocationHash("repo", "file.go", "Fn", core.CategoryLogic)

	f1 := makeFinding(pr.ID, task.ID, hash)
	err := st.CreateFinding(ctx, f1)
	require.NoError(t, err)

	// Same location_hash + same PR should violate the unique index.
	f2 := makeFinding(pr.ID, task.ID, hash)
	err = st.CreateFinding(ctx, f2)
	require.Error(t, err, "duplicate (location_hash, pull_request_id) should fail")
}

// ---------------------------------------------------------------------------
// DismissedFingerprint tests
// ---------------------------------------------------------------------------

func TestIsFingerprintDismissed(t *testing.T) {
	pool, st := setupTestDB(t)
	ctx := context.Background()

	hash := "abc123hash"
	repo := "ashita-ai/mimir"

	// Not dismissed yet.
	dismissed, err := st.IsFingerprintDismissed(ctx, hash, repo)
	require.NoError(t, err)
	assert.False(t, dismissed)

	// Insert a dismissal directly.
	_, err = pool.Exec(ctx,
		`INSERT INTO dismissed_fingerprints (fingerprint, repo_full_name, dismissed_by, reason) VALUES ($1, $2, $3, $4)`,
		hash, repo, "reviewer", "false positive",
	)
	require.NoError(t, err)

	// Now it should be dismissed.
	dismissed, err = st.IsFingerprintDismissed(ctx, hash, repo)
	require.NoError(t, err)
	assert.True(t, dismissed)

	// Different repo — not dismissed.
	dismissed, err = st.IsFingerprintDismissed(ctx, hash, "other/repo")
	require.NoError(t, err)
	assert.False(t, dismissed)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func insertTestPR(t *testing.T, st *store.Store) *core.PullRequest {
	t.Helper()
	pr := &core.PullRequest{
		GitHubPRID:   time.Now().UnixNano(), // unique per test
		RepoFullName: "ashita-ai/mimir",
		PRNumber:     1,
		HeadSHA:      uuid.New().String()[:8],
		BaseSHA:      "base000",
		Author:       "testuser",
		State:        core.PRStateOpen,
		Metadata:     map[string]any{},
	}
	require.NoError(t, st.UpsertPullRequest(context.Background(), pr))
	return pr
}

func insertTestTask(t *testing.T, st *store.Store, prID uuid.UUID) *core.ReviewTask {
	t.Helper()
	task := &core.ReviewTask{
		PullRequestID: prID,
		TaskType:      core.TaskTypeSecurity,
		FilePath:      "internal/auth/token.go",
		Symbol:        "ValidateToken",
		RiskScore:     0.8,
	}
	require.NoError(t, st.CreateReviewTask(context.Background(), task))
	return task
}

func insertTestFinding(t *testing.T, st *store.Store, prID, taskID uuid.UUID) *core.Finding {
	t.Helper()
	f := makeFinding(prID, taskID, core.ComputeLocationHash("ashita-ai/mimir", "file.go", "Fn", core.CategoryLogic))
	require.NoError(t, st.CreateFinding(context.Background(), f))
	return f
}

func makeFinding(prID, taskID uuid.UUID, locationHash string) *core.Finding {
	line := 10
	return &core.Finding{
		ReviewTaskID:    taskID,
		PullRequestID:   prID,
		FilePath:        "file.go",
		StartLine:       &line,
		Symbol:          "Fn",
		Category:        core.CategoryLogic,
		ConfidenceTier:  core.ConfidenceMedium,
		ConfidenceScore: 0.65,
		Severity:        core.SeverityMedium,
		Title:           "Test finding",
		Body:            "This is a test finding.",
		LocationHash:    locationHash,
		ModelID:         "test-model",
		Metadata:        map[string]any{},
	}
}
