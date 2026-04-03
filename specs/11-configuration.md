# Spec 11: Configuration

> **Status:** Draft
> **Date:** 2026-03-27

---

## Configuration Loading

Priority (highest wins): CLI flags → environment variables → defaults.

No config file in M1. Environment variables cover all knobs. M2 may add YAML config for complex deployments.

---

## Reference Table

### Required

| Env Var | Flag | Type | Description |
|---------|------|------|-------------|
| `DATABASE_URL` | — | string | PostgreSQL connection string. Example: `postgres://mimir:mimir@localhost:5432/mimir?sslmode=disable` |
| `MIMIR_MODEL_API_KEY` | — | string | Anthropic API key (or other provider key) |

Plus one of the following auth modes:

**GitHub App mode (recommended):**

| Env Var | Flag | Type | Description |
|---------|------|------|-------------|
| `MIMIR_GITHUB_APP_ID` | — | int64 | GitHub App ID |
| `MIMIR_GITHUB_APP_PRIVATE_KEY_PATH` | — | string | Path to PEM private key file |
| `MIMIR_GITHUB_WEBHOOK_SECRET` | — | string | Webhook signature secret |

**PAT mode (CLI fallback):**

| Env Var | Flag | Type | Description |
|---------|------|------|-------------|
| `MIMIR_GITHUB_TOKEN` | — | string | Personal access token |

### Optional — Server

| Env Var | Flag | Type | Default | Description |
|---------|------|------|---------|-------------|
| `MIMIR_LISTEN_ADDR` | `--listen-addr` | string | `:8080` | HTTP listen address |
| `MIMIR_WORKERS` | `--workers` | int | `5` | River worker goroutines. 0 = HTTP only |
| `MIMIR_HTTP_ENABLED` | `--http` | bool | `true` | Enable HTTP server. false = workers only |

### Optional — Runtime

| Env Var | Flag | Type | Default | Description |
|---------|------|------|---------|-------------|
| `MIMIR_MAX_CONCURRENT_TASKS` | `--max-concurrent` | int | `10` | Max parallel task executions per job |
| `MIMIR_TASK_TIMEOUT` | `--task-timeout` | duration | `60s` | Per-task context timeout |
| `MIMIR_JOB_TIMEOUT` | — | duration | `5m` | River job-level timeout |

### Optional — Policy

| Env Var | Flag | Type | Default | Description |
|---------|------|------|---------|-------------|
| `MIMIR_MAX_INLINE_FINDINGS` | — | int | `0` | Max inline comments per PR. 0 = no cap (all high-confidence findings post inline) |
| `MIMIR_CONFIDENCE_THRESHOLD_HIGH` | — | float64 | `0.80` | Score threshold for high confidence tier |
| `MIMIR_CONFIDENCE_THRESHOLD_MEDIUM` | — | float64 | `0.50` | Score threshold for medium confidence tier |

### Optional — Model Routing

| Env Var | Flag | Type | Default | Description |
|---------|------|------|---------|-------------|
| `MIMIR_MODEL_DEFAULT` | — | string | `claude-opus-4-6` | Default model for all task types |
| `MIMIR_MODEL_SECURITY` | — | string | `claude-opus-4-6` | Model for security tasks |
| `MIMIR_MODEL_LOGIC` | — | string | `claude-opus-4-6` | Model for logic tasks |
| `MIMIR_MODEL_TEST_COVERAGE` | — | string | `claude-sonnet-4-6` | Model for test coverage tasks |
| `MIMIR_MODEL_STYLE` | — | string | `claude-haiku-4-5-20251001` | Model for style tasks |

### Optional — Slice Budget

| Env Var | Flag | Type | Default | Description |
|---------|------|------|---------|-------------|
| `MIMIR_SLICE_MAX_TOKENS` | — | int | `16000` | Hard cap per task |
| `MIMIR_SLICE_DIFF_PCT` | — | int | `60` | Default diff hunk budget (%) |
| `MIMIR_SLICE_CALLGRAPH_PCT` | — | int | `25` | Default call graph budget (%) |
| `MIMIR_SLICE_TESTS_PCT` | — | int | `15` | Default test context budget (%) |

### Optional — Database

| Env Var | Flag | Type | Default | Description |
|---------|------|------|---------|-------------|
| `MIMIR_DB_MAX_CONNS` | — | int | `20` | pgx pool max connections |
| `MIMIR_DB_MIN_CONNS` | — | int | `2` | pgx pool min connections |

---

## Validation

At startup, before constructing any adapters:

```go
func validateConfig(cfg *Config) error {
    // Required
    if cfg.DatabaseURL == "" {
        return errors.New("DATABASE_URL is required")
    }
    if cfg.ModelAPIKey == "" {
        return errors.New("MIMIR_MODEL_API_KEY is required")
    }

    // Auth mode detection
    hasApp := cfg.GitHub.AppID != 0 && cfg.GitHub.PrivateKeyPath != ""
    hasPAT := cfg.GitHub.Token != ""
    if !hasApp && !hasPAT {
        return errors.New("GitHub credentials required: set MIMIR_GITHUB_APP_ID + MIMIR_GITHUB_APP_PRIVATE_KEY_PATH, or MIMIR_GITHUB_TOKEN")
    }
    if hasApp && cfg.GitHub.WebhookSecret == "" {
        return errors.New("MIMIR_GITHUB_WEBHOOK_SECRET is required in App mode")
    }

    // Budget percentages must sum to ≤ 100
    total := cfg.Slice.DiffPct + cfg.Slice.CallGraphPct + cfg.Slice.TestsPct
    if total > 100 {
        return fmt.Errorf("slice budget percentages sum to %d, must be ≤ 100", total)
    }

    return nil
}
```

Validation failures are fatal — exit 1 with a clear error message. Do not start the server with bad config.

---

## Environment for Local Development

```bash
# .env (gitignored)
DATABASE_URL=postgres://mimir:mimir@localhost:5432/mimir?sslmode=disable
MIMIR_MODEL_API_KEY=sk-ant-...
MIMIR_GITHUB_TOKEN=ghp_...
```

For local development with `mimir review`, only `DATABASE_URL`, `MIMIR_MODEL_API_KEY`, and `MIMIR_GITHUB_TOKEN` are needed. The full GitHub App config is for production `mimir serve` deployments.
