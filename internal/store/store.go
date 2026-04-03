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
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashita-ai/mimir/internal/core"
	"github.com/ashita-ai/mimir/internal/store/dbsqlc"
	"github.com/ashita-ai/mimir/pkg/adapter"
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
// Transactions
// ---------------------------------------------------------------------------

func (s *Store) WithTx(ctx context.Context, fn adapter.TxFunc) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	txStore := &Store{
		pool: s.pool,
		q:    s.q.WithTx(tx),
	}

	if err := fn(txStore, tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
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
		DeletedAt:    row.DeletedAt,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}

	if err := json.Unmarshal(row.Metadata, &pr.Metadata); err != nil {
		return nil, fmt.Errorf("store: unmarshal PR metadata: %w", err)
	}

	return pr, nil
}

func (s *Store) SoftDeletePullRequest(ctx context.Context, id uuid.UUID) error {
	n, err := s.q.SoftDeletePullRequest(ctx, id)
	if err != nil {
		return fmt.Errorf("store: soft delete pull request %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: pull request %s not found or already deleted", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PipelineRuns
// ---------------------------------------------------------------------------

func (s *Store) CreatePipelineRun(ctx context.Context, run *core.PipelineRun) error {
	if run.ID == uuid.Nil {
		run.ID = uuid.New()
	}
	if run.Status == "" {
		run.Status = core.PipelineRunStatusRunning
	}

	metadataJSON, err := json.Marshal(run.Metadata)
	if err != nil {
		return fmt.Errorf("store: marshal pipeline run metadata: %w", err)
	}

	result, err := s.q.CreatePipelineRun(ctx, dbsqlc.CreatePipelineRunParams{
		ID:            run.ID,
		PullRequestID: run.PullRequestID,
		HeadSha:       run.HeadSHA,
		Status:        string(run.Status),
		PromptVersion: run.PromptVersion,
		ConfigHash:    run.ConfigHash,
		Metadata:      metadataJSON,
	})
	if err != nil {
		return fmt.Errorf("store: create pipeline run: %w", err)
	}

	run.CreatedAt = result.CreatedAt
	run.UpdatedAt = result.UpdatedAt
	return nil
}

func (s *Store) CompletePipelineRun(ctx context.Context, id uuid.UUID, status core.PipelineRunStatus, taskCount, findingCount int, errMsg *string) error {
	n, err := s.q.CompletePipelineRun(ctx, dbsqlc.CompletePipelineRunParams{
		ID:           id,
		Status:       string(status),
		TaskCount:    int32(taskCount),
		FindingCount: int32(findingCount),
		Error:        textFromStringPtr(errMsg),
	})
	if err != nil {
		return fmt.Errorf("store: complete pipeline run %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: pipeline run %s not found", id)
	}
	return nil
}

func (s *Store) GetPipelineRun(ctx context.Context, id uuid.UUID) (*core.PipelineRun, error) {
	row, err := s.q.GetPipelineRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: get pipeline run %s: %w", id, err)
	}
	run, err := pipelineRunFromRow(row)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Store) ListPipelineRunsForPR(ctx context.Context, pullRequestID uuid.UUID) ([]core.PipelineRun, error) {
	rows, err := s.q.ListPipelineRunsForPR(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list pipeline runs for PR %s: %w", pullRequestID, err)
	}

	runs := make([]core.PipelineRun, len(rows))
	for i, row := range rows {
		run, err := pipelineRunFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("store: convert pipeline run row %d: %w", i, err)
		}
		runs[i] = run
	}
	return runs, nil
}

func (s *Store) ReconcileStalePipelineRuns(ctx context.Context) (int64, error) {
	n, err := s.q.ReconcileStalePipelineRuns(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: reconcile stale pipeline runs: %w", err)
	}
	return n, nil
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
		PipelineRunID: task.PipelineRunID,
		TaskType:      string(task.TaskType),
		FilePath:      task.FilePath,
		Symbol:        textFromString(task.Symbol),
		RiskScore:     float64(task.RiskScore),
		ModelID:       task.ModelID,
		DiffHunk:      textFromString(task.DiffHunk),
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
		PipelineRunID:    f.PipelineRunID,
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
		HeadSha:          f.HeadSHA,
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
	return findingsFromRows(rows)
}

func (s *Store) FindPriorFinding(ctx context.Context, locationHash, repoFullName string) (*core.Finding, error) {
	row, err := s.q.FindPriorFinding(ctx, dbsqlc.FindPriorFindingParams{
		LocationHash: locationHash,
		RepoFullName: repoFullName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: find prior finding: %w", err)
	}
	f, err := findingFromRow(row)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Store) ListUnaddressedFindings(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListUnaddressedFindings(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list unaddressed findings for PR %s: %w", pullRequestID, err)
	}
	return findingsFromRows(rows)
}

func (s *Store) ListUnpostedFindings(ctx context.Context, pullRequestID uuid.UUID) ([]core.Finding, error) {
	rows, err := s.q.ListUnpostedFindings(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list unposted findings for PR %s: %w", pullRequestID, err)
	}
	return findingsFromRows(rows)
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

// ---------------------------------------------------------------------------
// Dismissed Fingerprints
// ---------------------------------------------------------------------------

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

func (s *Store) DismissFingerprint(ctx context.Context, fingerprint, repoFullName, dismissedBy, reason string) error {
	return s.q.DismissFingerprint(ctx, dbsqlc.DismissFingerprintParams{
		Fingerprint:  fingerprint,
		RepoFullName: repoFullName,
		DismissedBy:  dismissedBy,
		Reason:       textFromString(reason),
	})
}

// ---------------------------------------------------------------------------
// Finding Events
// ---------------------------------------------------------------------------

func (s *Store) CreateFindingEvent(ctx context.Context, event *core.FindingEvent) error {
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}

	metadataJSON, err := json.Marshal(event.Metadata)
	if err != nil {
		return fmt.Errorf("store: marshal finding event metadata: %w", err)
	}

	createdAt, err := s.q.CreateFindingEvent(ctx, dbsqlc.CreateFindingEventParams{
		ID:        event.ID,
		FindingID: event.FindingID,
		EventType: event.EventType,
		Actor:     event.Actor,
		OldValue:  textFromString(event.OldValue),
		NewValue:  textFromString(event.NewValue),
		Metadata:  metadataJSON,
	})
	if err != nil {
		return fmt.Errorf("store: create finding event: %w", err)
	}

	event.CreatedAt = createdAt
	return nil
}

func (s *Store) ListEventsForFinding(ctx context.Context, findingID uuid.UUID) ([]core.FindingEvent, error) {
	rows, err := s.q.ListEventsForFinding(ctx, findingID)
	if err != nil {
		return nil, fmt.Errorf("store: list events for finding %s: %w", findingID, err)
	}

	events := make([]core.FindingEvent, len(rows))
	for i, row := range rows {
		event, err := findingEventFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("store: convert finding event row %d: %w", i, err)
		}
		events[i] = event
	}
	return events, nil
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
		PipelineRunID: row.PipelineRunID,
		TaskType:      core.TaskType(row.TaskType),
		FilePath:      row.FilePath,
		Symbol:        stringFromText(row.Symbol),
		RiskScore:     core.RiskScore(row.RiskScore),
		ModelID:       row.ModelID,
		DiffHunk:      stringFromText(row.DiffHunk),
		Status:        core.TaskStatus(row.Status),
		Error:         stringPtrFromText(row.Error),
		StartedAt:     row.StartedAt,
		CompletedAt:   row.CompletedAt,
		CreatedAt:     row.CreatedAt,
	}
}

// findingFromRow converts a sqlc-generated Finding to a core.Finding.
func findingFromRow(row dbsqlc.Finding) (core.Finding, error) {
	f := core.Finding{
		ID:                    row.ID,
		ReviewTaskID:          row.ReviewTaskID,
		PullRequestID:         row.PullRequestID,
		PipelineRunID:         row.PipelineRunID,
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
		HeadSHA:               row.HeadSha,
		PostedAt:              row.PostedAt,
		GitHubCommentID:       int64PtrFromInt8(row.GithubCommentID),
		AddressedInNextCommit: row.AddressedInNextCommit,
		SuppressionReason:     stringFromText(row.SuppressionReason),
		DismissedAt:           row.DismissedAt,
		DismissedBy:           stringFromText(row.DismissedBy),
		ModelID:               row.ModelID,
		PromptTokens:          intPtrFromInt4(row.PromptTokens),
		CompletionTokens:      intPtrFromInt4(row.CompletionTokens),
		CreatedAt:             row.CreatedAt,
		UpdatedAt:             row.UpdatedAt,
	}

	if row.Metadata != nil {
		if err := json.Unmarshal(row.Metadata, &f.Metadata); err != nil {
			return core.Finding{}, fmt.Errorf("unmarshal finding metadata: %w", err)
		}
	}

	return f, nil
}

// findingsFromRows converts a slice of sqlc Finding rows to core.Finding slice.
func findingsFromRows(rows []dbsqlc.Finding) ([]core.Finding, error) {
	findings := make([]core.Finding, len(rows))
	for i, row := range rows {
		f, err := findingFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("store: convert finding row %d: %w", i, err)
		}
		findings[i] = f
	}
	return findings, nil
}

// pipelineRunFromRow converts a sqlc-generated PipelineRun to a core.PipelineRun.
func pipelineRunFromRow(row dbsqlc.PipelineRun) (core.PipelineRun, error) {
	run := core.PipelineRun{
		ID:            row.ID,
		PullRequestID: row.PullRequestID,
		HeadSHA:       row.HeadSha,
		Status:        core.PipelineRunStatus(row.Status),
		PromptVersion: row.PromptVersion,
		ConfigHash:    row.ConfigHash,
		TaskCount:     int(row.TaskCount),
		FindingCount:  int(row.FindingCount),
		Error:         stringPtrFromText(row.Error),
		StartedAt:     row.StartedAt,
		CompletedAt:   row.CompletedAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}

	if row.Metadata != nil {
		if err := json.Unmarshal(row.Metadata, &run.Metadata); err != nil {
			return core.PipelineRun{}, fmt.Errorf("unmarshal pipeline run metadata: %w", err)
		}
	}

	return run, nil
}

// findingEventFromRow converts a sqlc-generated FindingEvent to a core.FindingEvent.
func findingEventFromRow(row dbsqlc.FindingEvent) (core.FindingEvent, error) {
	event := core.FindingEvent{
		ID:        row.ID,
		FindingID: row.FindingID,
		EventType: row.EventType,
		Actor:     row.Actor,
		OldValue:  stringFromText(row.OldValue),
		NewValue:  stringFromText(row.NewValue),
		CreatedAt: row.CreatedAt,
	}

	if row.Metadata != nil {
		if err := json.Unmarshal(row.Metadata, &event.Metadata); err != nil {
			return core.FindingEvent{}, fmt.Errorf("unmarshal finding event metadata: %w", err)
		}
	}

	return event, nil
}
