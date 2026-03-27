# ashita-ai/mimir

AI-powered code review pipeline. Ingests GitHub PRs, builds semantic context via tree-sitter, fans out review tasks to LLMs, and posts findings as inline comments.

## Tech stack

- **Language:** Go 1.23
- **Database:** PostgreSQL 16+ (`pgx/v5`, sqlc for query generation, goose for migrations)
- **Job queue:** River (PostgreSQL-backed, Go-native)
- **HTTP:** chi v5 router
- **Semantic index:** tree-sitter (Go full support; Python, TypeScript, PHP approximate)
- **LLM provider:** Anthropic (Claude Opus default, Sonnet for test coverage, Haiku for style)
- **CLI:** cobra
- **Logging:** zap (structured)
- **Testing:** stdlib `testing` + testify assertions

## Project structure

```
cmd/mimir/           Entrypoint. Config loading, adapter wiring, server startup.
pkg/adapter/         Interface types only. No implementations. Every other package depends on this.
internal/
  core/              Pure domain types. No I/O, no external deps. Changes here ripple everywhere.
  ingest/            GitHub App auth, webhook handler, PR fetching, comment posting, reaction polling.
  index/             Tree-sitter parsing, symbol extraction, change cone, import analysis.
  planner/           Task generation, risk scoring, model routing, task-level dedup.
  runtime/           Slice building, prompt assembly, bounded fan-out, model inference, circuit breaker.
  policy/            Confidence penalties, cross-run dedup, dismissals, triage, inline cap, posting retry.
  eval/              Scorecard queries, reaction-based feedback, replay data capture.
  store/             PostgreSQL schema (goose migrations), sqlc queries, connection pool, transactions.
specs/               M1 build specs (00-12). The source of truth for implementation.
adr/                 Architecture decision records (0001-0005). Accepted decisions.
scratchpad/          Working design docs. Not authoritative — specs supersede these.
```

**Dependency rule:** No package in `internal/` imports another `internal/` package. Cross-cutting dependencies flow through interfaces in `pkg/adapter`. The wiring layer in `cmd/mimir` constructs concrete implementations and injects them.

## Commands

**Build:**
```sh
go build ./...
```

**Test:**
```sh
go test -race -count=1 ./...
```

**Lint (when available):**
```sh
go vet ./...
```

**Database migrations:**
```sh
goose -dir internal/store/migrations postgres "$DATABASE_URL" up
```

**Generate sqlc:**
```sh
sqlc generate
```

## Spec lifecycle

Every spec in `specs/` carries a status in its frontmatter:

| Status | Meaning |
|--------|---------|
| `Draft` | Under active design. May change substantially. |
| `Reviewed` | Design-reviewed for correctness, durability, and audit trail. Ready for implementation. |
| `Implementing` | Implementation in progress. Spec is locked unless a bug is found. |
| `Implemented` | Code matches spec. Spec becomes documentation. |
| `Superseded` | Replaced by a newer spec. Link to successor in frontmatter. |

When changing a spec's status, update the `> **Status:**` line in the file header.

## Architecture invariants

These are non-negotiable. Violating any of these is a bug.

1. **No `ON DELETE CASCADE`.** All foreign keys use `ON DELETE RESTRICT`. Audit data is never silently destroyed.
2. **Persist before post.** Findings are written to the DB before posting to GitHub. If posting fails, findings survive.
3. **Append-only audit log.** Every finding state transition is recorded in `finding_events`. The finding row is current state; the events table is history.
4. **Soft deletes only.** Hard deletion requires explicit reverse-dependency-order operations and human review.
5. **No silent drops.** Every finding the model produces is persisted — winners and dedup losers alike. Losers get `suppression_reason`.
6. **synchronous_commit = on.** PostgreSQL must not be configured with `synchronous_commit = off`. This is a hard durability requirement.

## Code style

```go
// Constructor pattern: explicit dependencies, no globals, no init().
func NewGitHubProvider(cfg GitHubProviderConfig) *GitHubProvider

// Every method takes context first. Cancellation propagates everywhere.
func (s *PostgresStore) CreateFinding(ctx context.Context, f *core.Finding) error

// Wrap errors at boundaries. No raw I/O errors escape adapters.
return fmt.Errorf("github: fetch PR: %w", err)

// Confidence scores are rounded after arithmetic to prevent IEEE 754 artifacts.
score = roundConfidence(score * 0.85) // math.Round(score*10000) / 10000
```

## Boundaries

**Always:**
- Scope every finding query by `pull_request_id` or `pipeline_run_id`
- Record state transitions in `finding_events` before mutating the finding row
- Use `ON DELETE RESTRICT` on all foreign keys
- Use `context.Context` for all I/O operations
- Use structured logging (zap) — never `fmt.Print` or `log.*`
- Run `go build ./...` and `go vet ./...` before committing

**Never:**
- Add `ON DELETE CASCADE` to any foreign key
- Delete rows from `findings`, `finding_events`, or `pipeline_runs` in application code
- Store secrets in code, config files, or metadata JSONB columns
- Commit `.env` files
- Add `Co-Authored-By` trailers to commits
- Skip the persist-before-post invariant for any finding
- Use `synchronous_commit = off` in any environment

**Ask first:**
- Adding new direct dependencies to go.mod
- Changing the `finding_events` schema or event types
- Modifying adapter interfaces in `pkg/adapter`
- Any schema migration that drops or renames columns
- Changes to prompt templates (affects eval reproducibility)

## Pull requests

Every PR description MUST end with one of the following, chosen at your discretion:

**Option A — Song Parody.** 1-2 verses of a well-known song, rewritten to argue for your favorite programming language. Name the original song.

**Option B — Vegan Recipe.** A real, impressive vegan recipe (ingredient list + 2-3 sentence method). Not a joke — it should be something you'd actually want to eat.

**Option C — Fictional Debate.** 2-4 sentences of a fictional debate about the PR between two famous people (scientists, philosophers, writers, historical figures — not tech people).

**Option D — Non-Obvious Trivia.** A surprising, verifiable piece of math or science trivia that most engineers wouldn't know. Include enough detail to be independently verifiable.

Format as a blockquote. Be creative. Repeating a previous PR's choice is fine; repeating its content is not.

This is not optional. PRs missing the blockquote will be sent back.

## Conventions

- Commit messages: imperative mood, concise first line, body explains "why"
- Branch names: `{username}/{description}` for feature work
- Specs are the source of truth. If code disagrees with a spec, the spec wins (unless the spec has a bug — file a fix).
- ADRs are append-only. Don't edit accepted ADRs; write a new one that supersedes.
- Scratchpad docs are working notes. They are not authoritative and may be stale.
