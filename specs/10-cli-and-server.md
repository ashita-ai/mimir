# Spec 10: CLI & Server

> **Status:** Draft
> **Date:** 2026-03-27
> **Package:** `cmd/mimir`
> **ADR:** [0005-service-architecture.md](../adr/0005-service-architecture.md)

---

## Command Structure

```
mimir
├── serve     Start HTTP server + River workers (default command)
└── review    One-shot PR review (CLI mode)
```

Both commands share the same adapter construction and pipeline logic. The difference is how the pipeline is triggered (webhook vs. CLI argument) and where output goes (GitHub comments vs. stdout).

---

## `mimir serve`

### Startup Sequence

```go
func runServe(cfg Config) error {
    // 1. Connect to PostgreSQL
    pool, err := pgxpool.New(ctx, cfg.DatabaseURL)

    // 2. Run goose migrations
    goose.Up(pool, "internal/store/migrations")

    // 3. Run River migrations
    migrator := rivermigrate.New(riverpgxv5.New(pool))
    migrator.Migrate(ctx, rivermigrate.DirectionUp)

    // 4. Construct adapters
    store := store.NewPostgresStore(pool)
    provider := ingest.NewGitHubProvider(cfg.GitHub)
    index := index.NewTreeSitterIndex()
    models := buildModelRegistry(cfg.Models)  // map[string]adapter.ModelAdapter
    policy := policy.NewDefaultPolicy(store, cfg.Policy)

    // 5. Construct pipeline
    pipeline := pipeline.New(store, provider, index, models, policy, cfg.Runtime)

    // 6. Construct River client with job handlers
    riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
        Queues: map[string]river.QueueConfig{
            river.QueueDefault: {MaxWorkers: cfg.MaxWorkers}, // default: 5
        },
        Workers: workers,
    })

    // Register job handler
    river.AddWorker(workers, &ReviewPipelineWorker{pipeline: pipeline})

    // 7. Start River workers (unless --workers=0)
    if cfg.MaxWorkers > 0 {
        riverClient.Start(ctx)
    }

    // 8. Build chi router (unless --http=false)
    if cfg.HTTPEnabled {
        r := chi.NewRouter()
        r.Use(middleware.Logger)
        r.Use(middleware.Recoverer)
        r.Use(middleware.Timeout(30 * time.Second))

        r.Route("/v1", func(r chi.Router) {
            r.Get("/healthz", healthHandler(pool, riverClient))
            r.Post("/webhooks/github", webhookHandler(cfg.GitHub.WebhookSecret, store, riverClient))
        })

        srv := &http.Server{Addr: cfg.ListenAddr, Handler: r}
        go srv.ListenAndServe()
    }

    // 9. Check synchronous_commit setting
    var syncCommit string
    pool.QueryRow(ctx, "SHOW synchronous_commit").Scan(&syncCommit)
    if syncCommit != "on" {
        log.Warn("synchronous_commit is not 'on' — data loss is possible on crash",
            zap.String("synchronous_commit", syncCommit))
    }

    // 10. Reconcile stale pipeline runs from prior crashes
    store.ReconcileStalePipelineRuns(ctx, 2*cfg.JobTimeout) // default: 10 min

    // 11. Register periodic jobs
    riverClient.PeriodicJobs().Add(
        river.NewPeriodicJob(
            river.PeriodicInterval(15*time.Minute),
            func() (river.JobArgs, *river.InsertOpts) {
                return &ReactionPollJob{}, nil
            },
            nil,
        ),
    )

    riverClient.PeriodicJobs().Add(
        river.NewPeriodicJob(
            river.PeriodicInterval(5*time.Minute),
            func() (river.JobArgs, *river.InsertOpts) {
                return &PostingRetryJob{}, nil
            },
            nil,
        ),
    )

    riverClient.PeriodicJobs().Add(
        river.NewPeriodicJob(
            river.PeriodicInterval(10*time.Minute),
            func() (river.JobArgs, *river.InsertOpts) {
                return &ReconcileStaleRunsJob{Timeout: 2 * cfg.JobTimeout}, nil
            },
            nil,
        ),
    )

    // 12. Wait for shutdown signal
    <-ctx.Done()
    // Graceful shutdown: stop HTTP → drain River → close pool
}
```

### Graceful Shutdown

```
1. Receive SIGINT or SIGTERM
2. Stop accepting new HTTP requests (server.Shutdown with 10s timeout)
3. Stop River from picking up new jobs (riverClient.Stop with 30s timeout)
4. Wait for in-flight jobs to complete (up to 30s)
5. Close database pool
6. Exit
```

In-flight River jobs that don't complete within the drain timeout are re-enqueued by River's at-least-once delivery on the next startup.

### Flags

```
mimir serve [flags]

Flags:
  --listen-addr string    HTTP listen address (default ":8080")
  --workers int           Number of River worker goroutines (default 5; 0 = HTTP only)
  --http                  Enable HTTP server (default true; false = workers only)
  --max-concurrent int    Max concurrent tasks per job (default 10)
  --task-timeout duration Per-task timeout (default 60s)
```

All flags have corresponding environment variables (see `specs/11-configuration.md`).

---

## `mimir review`

One-shot CLI mode. Reviews a single PR and prints findings to stdout.

### Usage

```
mimir review --repo owner/repo --pr 123 [--format json|table] [flags]
```

### Flow

```go
func runReview(cfg Config) error {
    // 1. Connect to PostgreSQL (same as serve)
    pool, err := pgxpool.New(ctx, cfg.DatabaseURL)

    // 2. Construct adapters (same as serve, but uses PAT if no App credentials)
    store := store.NewPostgresStore(pool)
    provider := ingest.NewGitHubProvider(cfg.GitHub)
    index := index.NewTreeSitterIndex()
    models := buildModelRegistry(cfg.Models)
    policy := policy.NewDefaultPolicy(store, cfg.Policy)

    // 3. Run pipeline synchronously (no River).
    // pipeline.Run internally calls checkoutRepo to create a shallow clone,
    // runs the full pipeline, and cleans up the checkout directory on return.
    pipeline := pipeline.New(store, provider, index, models, policy, cfg.Runtime)
    result, err := pipeline.Run(ctx, cfg.RepoFullName, cfg.PRNumber)

    // 4. Output results
    switch cfg.OutputFormat {
    case "json":
        json.NewEncoder(os.Stdout).Encode(result)
    case "table":
        printTable(result)
    }

    // 5. Optionally post to GitHub (--post flag)
    if cfg.PostToGitHub {
        pipeline.PostResults(ctx, result)
    }
}
```

### Flags

```
mimir review [flags]

Required:
  --repo string    Repository full name (owner/repo)
  --pr int         Pull request number

Optional:
  --format string  Output format: json, table (default "table")
  --post           Post results to GitHub as comments (default false)
```

### Table Output Format

```
┌──────────────────────────┬──────┬──────────┬──────────┬────────────────────────────────┐
│ File                     │ Line │ Category │ Severity │ Title                          │
├──────────────────────────┼──────┼──────────┼──────────┼────────────────────────────────┤
│ internal/auth/handler.go │ 42   │ security │ critical │ SQL injection via unsanitized...│
│ internal/api/routes.go   │ 108  │ logic    │ medium   │ Nil pointer dereference on...  │
└──────────────────────────┴──────┴──────────┴──────────┴────────────────────────────────┘

2 findings (1 critical, 0 high, 1 medium, 0 low, 0 info)
1/3 functions reviewed (0 tasks failed)
```

---

## Health Check

`GET /v1/healthz` — returns 200 if the system is operational.

```json
{
  "status": "ok",
  "db": "connected",
  "workers": "running",
  "version": "0.1.0"
}
```

Checks:
1. `pool.Ping(ctx)` — PostgreSQL is reachable
2. River client state — workers are running (not stopped/shutting down)

Returns 503 if either check fails.

---

## Webhook Handler Detail

```go
func webhookHandler(secret string, store adapter.StoreAdapter, riverClient *river.Client) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // 1. Read body
        body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit
        if err != nil {
            http.Error(w, "failed to read body", http.StatusBadRequest)
            return
        }

        // 2. Verify signature
        sig := r.Header.Get("X-Hub-Signature-256")
        if err := verifySignature([]byte(secret), body, sig); err != nil {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }

        // 3. Check event type
        eventType := r.Header.Get("X-GitHub-Event")
        if eventType != "pull_request" {
            w.WriteHeader(http.StatusOK) // acknowledge but ignore
            return
        }

        // 4. Parse payload
        var event github.PullRequestEvent
        if err := json.Unmarshal(body, &event); err != nil {
            http.Error(w, "invalid payload", http.StatusBadRequest)
            return
        }

        // 5. Filter action
        action := event.GetAction()
        if action != "opened" && action != "synchronize" && action != "reopened" {
            w.WriteHeader(http.StatusOK)
            return
        }

        // 6. Transactional: upsert PR + enqueue job.
        // WithTx provides a tx-bound store (txStore) so that the PR upsert
        // and the River job insert execute on the SAME transaction.
        // If either fails, both are rolled back — no orphaned PRs, no lost jobs.
        ctx := r.Context()
        if err := store.WithTx(ctx, func(txStore adapter.StoreAdapter, tx pgx.Tx) error {
            pr := mapGitHubEventToPR(event)
            if err := txStore.UpsertPullRequest(ctx, pr); err != nil {
                return fmt.Errorf("upsert PR: %w", err)
            }

            _, err := riverClient.InsertTx(ctx, tx, &ReviewPipelineJob{
                PullRequestID: pr.ID,
            }, nil)
            return err
        }); err != nil {
            log.Error("webhook processing failed", zap.Error(err))
            http.Error(w, "internal error", http.StatusInternalServerError)
            return
        }

        w.WriteHeader(http.StatusAccepted)
    }
}
```

---

## River Job Definition

```go
type ReviewPipelineJob struct {
    PullRequestID uuid.UUID `json:"pull_request_id"`
}

func (ReviewPipelineJob) Kind() string { return "review_pipeline" }

type ReviewPipelineWorker struct {
    river.WorkerDefaults[ReviewPipelineJob]
    pipeline *pipeline.Pipeline
}

func (w *ReviewPipelineWorker) Work(ctx context.Context, job *river.Job[ReviewPipelineJob]) error {
    return w.pipeline.Run(ctx, job.Args.PullRequestID)
}
```

River configuration:
- Max attempts: 3 (default)
- Backoff: exponential (River default: 1s, 2s, 4s, ...)
- Job timeout: 5 minutes (pipeline-level; individual tasks have their own 60s timeout)

---

## Periodic Jobs

| Job | Interval | Purpose |
|-----|----------|---------|
| `ReactionPollJob` | 15 min | Poll GitHub reactions on posted findings for eval signal |
| `PostingRetryJob` | 5 min | Retry posting for findings persisted but not posted (GitHub API failure recovery) |
| `ReconcileStaleRunsJob` | 10 min | Mark orphaned `running` pipeline_runs as `failed` after 2× job timeout |

All periodic jobs are registered at startup. They are no-ops when there is no work to do (no unposted findings, no stale runs, no recent findings to poll).

---

## Model Registry

The wiring layer constructs one `ModelAdapter` per configured model and indexes them by ID:

```go
func buildModelRegistry(cfg ModelsConfig) map[string]adapter.ModelAdapter {
    registry := make(map[string]adapter.ModelAdapter)

    for _, m := range cfg.Models {
        switch m.Provider {
        case "anthropic":
            registry[m.ID] = runtime.NewAnthropicModel(runtime.AnthropicModelConfig{
                APIKey:    cfg.AnthropicAPIKey,
                ModelID:   m.ID,
                MaxTokens: m.MaxTokens,
            })
        // M2: case "openai", "google"
        }
    }

    return registry
}
```

The runtime looks up the model for each task via `registry[task.ModelID]`. If the model ID is not in the registry, the task fails with a config error (non-retryable).
