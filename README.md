# Mimir

Open-source AI PR review harness. Builds semantic context slices per changed function and routes them through configurable model and static analysis pipelines.

## Status

Early development (M1 in progress). Not yet production-ready.

## Architecture

- **Language:** Go 1.23 (ADR-0001)
- **Database:** PostgreSQL 16+ (ADR-0002)
- **Job queue:** riverqueue/river (ADR-0003)
- **Semantic index:** Tree-sitter, heuristic (ADR-0004)
- **Service mode:** Single binary, `mimir serve` (ADR-0005)

See `adr/` for locked decisions and `scratchpad/` for in-progress design notes.

## Local Development

**Prerequisites:** Go 1.23+, Docker

```bash
# Start PostgreSQL
docker compose up -d

# Build
make build

# Run migrations (none yet in M1)
make migrate-up

# Run one-shot review (stub)
./bin/mimir review --repo owner/repo --pr 123

# Start webhook server + workers (stub)
./bin/mimir serve
```

## Repository Structure

```
adr/          Accepted Decision Records (immutable once accepted)
scratchpad/   In-progress design notes and open questions
cmd/mimir/    CLI entry point (cobra)
internal/     Private implementation packages
  core/       Domain types (no I/O)
  ingest/     GitHub PR fetching and comment posting
  index/      Semantic repo map (tree-sitter)
  planner/    ReviewTask generation and risk scoring
  runtime/    Model + tool execution
  policy/     Finding filter and escalation
  eval/       Metrics and replay
  store/      DB layer (sqlc + goose migrations)
pkg/adapter/  Exported plugin interface types
docker/       Dockerfile
```

## License

Apache 2.0
