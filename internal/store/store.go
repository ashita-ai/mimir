// Package store implements the StoreAdapter interface (pkg/adapter) against
// PostgreSQL using sqlc-generated query functions and pgx/v5.
//
// The Store type is a thin wrapper that translates between core domain types
// and sqlc's generated pgtype-based structs. All SQL lives in queries/*.sql
// and is compiled by sqlc into dbsqlc/*.go.
package store

//go:generate sqlc generate -f ../../sqlc.yaml

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashita-ai/mimir/internal/core"
	"github.com/ashita-ai/mimir/internal/store/dbsqlc"
)

// Store implements pkg/adapter.StoreAdapter against PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
	q    *dbsqlc.Queries
}

// New creates a Store backed by the given connection pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{
		pool: pool,
		q:    dbsqlc.New(pool),
	}
}

// Pool exposes the underlying pool for use by other components (e.g. River)
// that need a raw pgxpool connection.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// ---------------------------------------------------------------------------
// PullRequests
// ---------------------------------------------------------------------------

func (s *Store) UpsertPullRequest(ctx context.Context, pr *core.PullRequest) error {
	if pr.ID == uuid.Nil {
		pr.ID = uuid.New()
	}

	metadataJSON, err := json.Marshal(pr.Metadata)
	if err != nil {
		return fmt.Errorf("store: marshal PR metadata: %w", err)
	}

	row, err := s.q.UpsertPullRequest(ctx, dbsqlc.UpsertPullRequestParams{
		ID:           pr.ID,
		GithubPrID:   pr.GitHubPRID,
		RepoFullName: pr.RepoFullName,
		PrNumber:     int32(pr.PRNumber),
		HeadSha:      pr.HeadSHA,
		BaseSha:      pr.BaseSHA,
		Author:       pr.Author,
		State:        string(pr.State),
		Metadata:     metadataJSON,
	})
	if err != nil {
		return fmt.Errorf("store: upsert pull request: %w", err)
	}

	pr.ID = row.ID
	pr.CreatedAt = row.CreatedAt
	pr.UpdatedAt = row.UpdatedAt
	return nil
}

func (s *Store) GetPullRequest(ctx context.Context, id uuid.UUID) (*core.PullRequest, error) {
	row, err := s.q.GetPullRequest(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: get pull request %s: %w", id, err)
	}

	pr := &core.PullRequest{
		ID:           row.ID,
		GitHubPRID:   row.GithubPrID,
		RepoFullName: row.RepoFullName,
		PRNumber:     int(row.PrNumber),
		HeadSHA:      row.HeadSha,
		BaseSHA:      row.BaseSha,
		Author:       row.Author,
		State:        core.PRState(row.State),
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}

	if err := json.Unmarshal(row.Metadata, &pr.Metadata); err != nil {
		return nil, fmt.Errorf("store: unmarshal PR metadata: %w", err)
	}

	return pr, nil
}

// ---------------------------------------------------------------------------
// ReviewTasks
// ---------------------------------------------------------------------------

func (s *Store) CreateReviewTask(ctx context.Context, task *core.ReviewTask) error {
	if task.ID == uuid.Nil {
		task.ID = uuid.New()
	}
	if task.Status == "" {
		task.Status = core.TaskStatusPending
	}

	createdAt, err := s.q.CreateReviewTask(ctx, dbsqlc.CreateReviewTaskParams{
		ID:            task.ID,
		PullRequestID: task.PullRequestID,
		TaskType:      string(task.TaskType),
		FilePath:      task.FilePath,
		Symbol:        textFromString(task.Symbol),
		RiskScore:     float64(task.RiskScore),
		Status:        string(task.Status),
	})
	if err != nil {
		return fmt.Errorf("store: create review task: %w", err)
	}

	task.CreatedAt = createdAt
	return nil
}

func (s *Store) UpdateReviewTaskStatus(ctx context.Context, id uuid.UUID, status core.TaskStatus, errMsg *string) error {
	var (
		n   int64
		err error
	)

	switch status {
	case core.TaskStatusRunning:
		n, err = s.q.UpdateReviewTaskStatusRunning(ctx, dbsqlc.UpdateReviewTaskStatusRunningParams{
			ID:     id,
			Status: string(status),
		})
	case core.TaskStatusCompleted, core.TaskStatusFailed:
		n, err = s.q.UpdateReviewTaskStatusTerminal(ctx, dbsqlc.UpdateReviewTaskStatusTerminalParams{
			ID:     id,
			Status: string(status),
			Error:  textFromStringPtr(errMsg),
		})
	default:
		n, err = s.q.UpdateReviewTaskStatusOther(ctx, dbsqlc.UpdateReviewTaskStatusOtherParams{
			ID:     id,
			Status: string(status),
		})
	}

	if err != nil {
		return fmt.Errorf("store: update review task status %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: review task %s not found", id)
	}
	return nil
}

func (s *Store) ListPendingReviewTasks(ctx context.Context) ([]core.ReviewTask, error) {
	rows, err := s.q.ListPendingReviewTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list pending review tasks: %w", err)
	}

	tasks := make([]core.ReviewTask, len(rows))
	for i, row := range rows {
		tasks[i] = reviewTaskFromRow(row)
	}
	return tasks, nil
}

// ---------------------------------------------------------------------------
// Findings
// ---------------------------------------------------------------------------

func (s *Store) CreateFinding(ctx context.Context, f *core.Finding) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}

	metadataJSON, err := json.Marshal(f.Metadata)
	if err != nil {
		return fmt.Errorf("store: marshal finding metadata: %w", err)
	}

	result, err := s.q.CreateFinding(ctx, dbsqlc.CreateFindingParams{
		ID:               f.ID,
		ReviewTaskID:     f.ReviewTaskID,
		PullRequestID:    f.PullRequestID,
		FilePath:         f.FilePath,
		StartLine:        int4FromIntPtr(f.StartLine),
		EndLine:          int4FromIntPtr(f.EndLine),
		Symbol:           textFromString(f.Symbol),
		Category:         string(f.Category),
		ConfidenceTier:   string(f.ConfidenceTier),
		ConfidenceScore:  f.ConfidenceScore,
		Severity:         string(f.Severity),
		Title:            f.Title,
		Body:             f.Body,
		Suggestion:       textFromString(f.Suggestion),
		LocationHash:     f.LocationHash,
		ContentHash:      textFromString(f.ContentHash),
		ModelID:          f.ModelID,
		PromptTokens:     int4FromIntPtr(f.PromptTokens),
		CompletionTokens: int4FromIntPtr(f.CompletionTokens),
		Metadata:         metadataJSON,
	})
	if err != nil {
		return fmt.Errorf("store: create finding: %w", err)
	}

	f.CreatedAt = result.CreatedAt
	f.UpdatedAt = result.UpdatedAt
	return nil
}

func (s *Store) ListFindingsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListFindingsForPR(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list findings for PR %s: %w", pullRequestID, err)
	}

	findings := make([]core.Finding, len(rows))
	for i, row := range rows {
		findings[i] = findingFromRow(row)
	}
	return findings, nil
}

func (s *Store) MarkFindingPosted(ctx context.Context, id uuid.UUID, commentID int64) error {
	n, err := s.q.MarkFindingPosted(ctx, dbsqlc.MarkFindingPostedParams{
		ID:              id,
		GithubCommentID: pgtype.Int8{Int64: commentID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("store: mark finding posted %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: finding %s not found", id)
	}
	return nil
}

func (s *Store) MarkFindingAddressed(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.MarkFindingAddressed(ctx, id)
	if err != nil {
		return fmt.Errorf("store: mark finding addressed %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: finding %s not found", id)
	}
	return nil
}

func (s *Store) IsFingerprintDismissed(ctx context.Context, locationHash, repoFullName string) (bool, error) {
	dismissed, err := s.q.IsFingerprintDismissed(ctx, dbsqlc.IsFingerprintDismissedParams{
		Fingerprint:  locationHash,
		RepoFullName: repoFullName,
	})
	if err != nil {
		return false, fmt.Errorf("store: check dismissed fingerprint: %w", err)
	}
	return dismissed, nil
}

// ---------------------------------------------------------------------------
// pgtype ↔ core conversion helpers
// ---------------------------------------------------------------------------

func textFromString(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: s, Valid: true}
}

func textFromStringPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func stringFromText(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func stringPtrFromText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func int4FromIntPtr(p *int) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{Valid: false}
	}
	return pgtype.Int4{Int32: int32(*p), Valid: true}
}

func intPtrFromInt4(i pgtype.Int4) *int {
	if !i.Valid {
		return nil
	}
	v := int(i.Int32)
	return &v
}

func int64PtrFromInt8(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	return &i.Int64
}

// reviewTaskFromRow converts a sqlc-generated ReviewTask to a core.ReviewTask.
func reviewTaskFromRow(row dbsqlc.ReviewTask) core.ReviewTask {
	return core.ReviewTask{
		ID:            row.ID,
		PullRequestID: row.PullRequestID,
		TaskType:      core.TaskType(row.TaskType),
		FilePath:      row.FilePath,
		Symbol:        stringFromText(row.Symbol),
		RiskScore:     core.RiskScore(row.RiskScore),
		Status:        core.TaskStatus(row.Status),
		Error:         stringPtrFromText(row.Error),
		StartedAt:     row.StartedAt,
		CompletedAt:   row.CompletedAt,
		CreatedAt:     row.CreatedAt,
	}
}

// findingFromRow converts a sqlc-generated Finding to a core.Finding.
func findingFromRow(row dbsqlc.Finding) core.Finding {
	f := core.Finding{
		ID:                    row.ID,
		ReviewTaskID:          row.ReviewTaskID,
		PullRequestID:         row.PullRequestID,
		FilePath:              row.FilePath,
		StartLine:             intPtrFromInt4(row.StartLine),
		EndLine:               intPtrFromInt4(row.EndLine),
		Symbol:                stringFromText(row.Symbol),
		Category:              core.Category(row.Category),
		ConfidenceTier:        core.ConfidenceTier(row.ConfidenceTier),
		ConfidenceScore:       row.ConfidenceScore,
		Severity:              core.Severity(row.Severity),
		Title:                 row.Title,
		Body:                  row.Body,
		Suggestion:            stringFromText(row.Suggestion),
		LocationHash:          row.LocationHash,
		ContentHash:           stringFromText(row.ContentHash),
		PostedAt:              row.PostedAt,
		GitHubCommentID:       int64PtrFromInt8(row.GithubCommentID),
		AddressedInNextCommit: row.AddressedInNextCommit,
		DismissedAt:           row.DismissedAt,
		DismissedBy:           stringFromText(row.DismissedBy),
		ModelID:               row.ModelID,
		PromptTokens:          intPtrFromInt4(row.PromptTokens),
		CompletionTokens:      intPtrFromInt4(row.CompletionTokens),
		CreatedAt:             row.CreatedAt,
		UpdatedAt:             row.UpdatedAt,
	}

	if row.Metadata != nil {
		_ = json.Unmarshal(row.Metadata, &f.Metadata)
	}

	return f
}
