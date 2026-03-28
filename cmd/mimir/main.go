package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver for database/sql

	"github.com/ashita-ai/mimir/internal/ingest"
	"github.com/ashita-ai/mimir/internal/queue"
	"github.com/ashita-ai/mimir/internal/store"
)

const defaultDatabaseURL = "postgres://mimir:mimir@localhost:5433/mimir?sslmode=disable"

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := newRootCmd(log).ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newRootCmd(log *zap.Logger) *cobra.Command {
	root := &cobra.Command{
		Use:   "mimir",
		Short: "AI-powered PR review harness",
		Long: `Mimir is an open-source AI PR review harness that builds semantic context
slices per changed function and routes them through configurable model and
static analysis pipelines to produce high-signal, low-noise code review findings.`,
		SilenceUsage: true,
	}

	root.AddCommand(newServeCmd(log))
	root.AddCommand(newReviewCmd(log))
	root.AddCommand(newMigrateCmd(log))

	return root
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func newServeCmd(log *zap.Logger) *cobra.Command {
	var (
		addr       string
		workers    int
		enableHTTP bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the webhook receiver and pipeline workers",
		Long: `serve starts a chi HTTP server to receive GitHub webhook events and
one or more river worker pools to process review jobs. Both run in the same
process. Scale horizontally by running multiple instances against the same DB.

Use --http=false to run workers only (no HTTP server).
Use --workers=0 to run HTTP only (no workers, useful behind a load balancer).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !enableHTTP && workers == 0 {
				return fmt.Errorf("nothing to run: both HTTP and workers are disabled")
			}

			ctx := cmd.Context()
			dbURL := envOrDefault("DATABASE_URL", defaultDatabaseURL)

			// --- pgx pool ---
			pool, err := pgxpool.New(ctx, dbURL)
			if err != nil {
				return fmt.Errorf("connect to database: %w", err)
			}
			defer pool.Close()

			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("ping database: %w", err)
			}
			log.Info("connected to database")

			st := store.New(pool)

			// --- river client (always created for job insertion; workers optional) ---
			riverWorkers := river.NewWorkers()
			river.AddWorker(riverWorkers, &queue.ReviewWorker{
				Store:  st,
				Logger: log.Named("review-worker"),
			})

			riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
				Queues: map[string]river.QueueConfig{
					"review": {MaxWorkers: workers},
				},
				Workers:     riverWorkers,
				MaxAttempts: 4, // ADR-0003: 3 retries
			})
			if err != nil {
				return fmt.Errorf("create river client: %w", err)
			}

			if workers > 0 {
				if err := riverClient.Start(ctx); err != nil {
					return fmt.Errorf("start river client: %w", err)
				}
				log.Info("river workers started", zap.Int("workers", workers))
			} else {
				log.Info("workers disabled (--workers=0), HTTP-only mode")
			}

			// --- HTTP server (optional via --http flag) ---
			var srv *http.Server

			errCh := make(chan error, 1)
			if enableHTTP {
				webhookSecret := os.Getenv("MIMIR_WEBHOOK_SECRET")
				if webhookSecret == "" {
					return fmt.Errorf("MIMIR_WEBHOOK_SECRET is required when HTTP is enabled (set --http=false for worker-only mode)")
				}

				r := chi.NewRouter()
				r.Use(middleware.RequestID)
				r.Use(middleware.RealIP)
				r.Use(middleware.Recoverer)

				r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
					if err := pool.Ping(r.Context()); err != nil {
						http.Error(w, "db unreachable", http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, "ok")
				})

				r.Mount("/webhooks/github", &ingest.WebhookHandler{
					OnPREvent: func(reqCtx context.Context, event ingest.PREvent) error {
						_, err := riverClient.Insert(reqCtx, queue.ReviewJobArgs{
							RepoFullName: event.RepoFullName,
							PRNumber:     event.PRNumber,
							GitHubPRID:   event.GitHubPRID,
							HeadSHA:      event.HeadSHA,
							BaseSHA:      event.BaseSHA,
							Author:       event.Author,
						}, nil)
						return err
					},
					Secret: []byte(webhookSecret),
					Logger: log.Named("webhook"),
				})

				srv = &http.Server{
					Addr:         addr,
					Handler:      r,
					ReadTimeout:  10 * time.Second,
					WriteTimeout: 30 * time.Second,
					IdleTimeout:  60 * time.Second,
				}

				go func() {
					log.Info("HTTP server listening", zap.String("addr", addr))
					if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						errCh <- err
					}
					close(errCh)
				}()
			} else {
				log.Info("HTTP disabled (--http=false), worker-only mode")
				close(errCh)
			}

			// Block until shutdown signal or server error.
			select {
			case <-ctx.Done():
				log.Info("shutdown signal received")
			case err := <-errCh:
				if err != nil {
					return fmt.Errorf("HTTP server error: %w", err)
				}
				// errCh closed without error (HTTP disabled) — wait for signal.
				<-ctx.Done()
				log.Info("shutdown signal received")
			}

			// Graceful shutdown.
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer shutdownCancel()

			if srv != nil {
				log.Info("shutting down HTTP server")
				if err := srv.Shutdown(shutdownCtx); err != nil {
					log.Error("HTTP shutdown error", zap.Error(err))
				}
			}

			if workers > 0 {
				log.Info("stopping river workers")
				if err := riverClient.Stop(shutdownCtx); err != nil {
					log.Error("river shutdown error", zap.Error(err))
				}
			}

			log.Info("shutdown complete")
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "HTTP listen address")
	cmd.Flags().IntVar(&workers, "workers", 4, "number of river worker goroutines")
	cmd.Flags().BoolVar(&enableHTTP, "http", true, "enable HTTP webhook receiver (set false for worker-only mode)")

	return cmd
}

// ---------------------------------------------------------------------------
// review (one-shot)
// ---------------------------------------------------------------------------

func newReviewCmd(log *zap.Logger) *cobra.Command {
	var (
		repo     string
		prNumber int
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a one-shot review of a single PR",
		Long: `review fetches a PR, runs the full analysis pipeline, and prints findings
to stdout. Does not require a running server. Useful for local development and CI.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required (e.g. ashita-ai/mimir)")
			}
			if prNumber <= 0 {
				return fmt.Errorf("--pr must be a positive integer")
			}
			log.Info("review not yet implemented",
				zap.String("repo", repo),
				zap.Int("pr", prNumber),
			)
			// TODO(M1): wire one-shot review pipeline
			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "repository in owner/name format")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "pull request number")

	return cmd
}

// ---------------------------------------------------------------------------
// migrate
// ---------------------------------------------------------------------------

func newMigrateCmd(log *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run database migrations",
	}

	cmd.AddCommand(newMigrateUpCmd(log))
	cmd.AddCommand(newMigrateDownCmd(log))
	cmd.AddCommand(newMigrateStatusCmd(log))

	return cmd
}

func newMigrateUpCmd(log *zap.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations (app schema + river tables)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dbURL := envOrDefault("DATABASE_URL", defaultDatabaseURL)

			// --- River migrations (must run first — river tables are independent) ---
			pool, err := pgxpool.New(ctx, dbURL)
			if err != nil {
				return fmt.Errorf("connect to database: %w", err)
			}
			defer pool.Close()

			riverMigrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
			if err != nil {
				return fmt.Errorf("create river migrator: %w", err)
			}

			riverRes, err := riverMigrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
			if err != nil {
				return fmt.Errorf("river migrate up: %w", err)
			}
			for _, v := range riverRes.Versions {
				log.Info("river migration applied", zap.Int("version", v.Version))
			}
			if len(riverRes.Versions) == 0 {
				log.Info("river migrations already up to date")
			}

			// --- App migrations via goose ---
			db, err := sql.Open("pgx", dbURL)
			if err != nil {
				return fmt.Errorf("open sql connection for goose: %w", err)
			}
			defer db.Close()

			migrationFS, err := fs.Sub(store.Migrations, "migrations")
			if err != nil {
				return fmt.Errorf("sub migrations fs: %w", err)
			}

			provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationFS)
			if err != nil {
				return fmt.Errorf("create goose provider: %w", err)
			}

			results, err := provider.Up(ctx)
			if err != nil {
				return fmt.Errorf("goose migrate up: %w", err)
			}

			for _, r := range results {
				log.Info("app migration applied",
					zap.Int64("version", r.Source.Version),
					zap.String("file", r.Source.Path),
					zap.Duration("duration", r.Duration),
				)
			}
			if len(results) == 0 {
				log.Info("app migrations already up to date")
			}

			return nil
		},
	}
}

func newMigrateDownCmd(log *zap.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Roll back the last app migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dbURL := envOrDefault("DATABASE_URL", defaultDatabaseURL)

			db, err := sql.Open("pgx", dbURL)
			if err != nil {
				return fmt.Errorf("open sql connection: %w", err)
			}
			defer db.Close()

			migrationFS, err := fs.Sub(store.Migrations, "migrations")
			if err != nil {
				return fmt.Errorf("sub migrations fs: %w", err)
			}

			provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationFS)
			if err != nil {
				return fmt.Errorf("create goose provider: %w", err)
			}

			result, err := provider.Down(ctx)
			if err != nil {
				return fmt.Errorf("goose migrate down: %w", err)
			}

			if result != nil {
				log.Info("app migration rolled back",
					zap.Int64("version", result.Source.Version),
					zap.Duration("duration", result.Duration),
				)
			} else {
				log.Info("no migrations to roll back")
			}

			return nil
		},
	}
}

func newMigrateStatusCmd(log *zap.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show migration status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dbURL := envOrDefault("DATABASE_URL", defaultDatabaseURL)

			db, err := sql.Open("pgx", dbURL)
			if err != nil {
				return fmt.Errorf("open sql connection: %w", err)
			}
			defer db.Close()

			migrationFS, err := fs.Sub(store.Migrations, "migrations")
			if err != nil {
				return fmt.Errorf("sub migrations fs: %w", err)
			}

			provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationFS)
			if err != nil {
				return fmt.Errorf("create goose provider: %w", err)
			}

			statuses, err := provider.Status(ctx)
			if err != nil {
				return fmt.Errorf("goose status: %w", err)
			}

			for _, s := range statuses {
				state := "pending"
				if s.State == goose.StateApplied {
					state = "applied"
				}
				log.Info("migration",
					zap.String("state", state),
					zap.Int64("version", s.Source.Version),
					zap.String("file", s.Source.Path),
				)
			}

			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

