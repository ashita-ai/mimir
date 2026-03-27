# ADR-0004: Semantic Index — Tree-sitter (M1)

> **Status:** Under discussion
> **Original decision date:** 2026-03-18. Significant design additions 2026-03-21.

---

## Context

Mimir's review pipeline needs code context beyond the raw diff: call sites, callers, type definitions, and test coverage for changed functions. A full LSP integration (real go-to-definition, cross-file type resolution) is the gold standard but represents multiple months of engineering work.

M1 needs a working semantic index. The question is how approximate it can be while still providing meaningful lift over raw-diff-only context.

---

## Decision

**Tree-sitter (`smacker/go-tree-sitter`) for M1**, with heuristic import and usage analysis.

- Parse each file's AST with tree-sitter to extract function definitions, type definitions, and import statements
- Heuristic cross-file usage: match function/type names from changed files against their occurrences in other files (text-search, not type-resolution)
- All output from the semantic index layer is **explicitly labeled as approximate** in any user-facing output or prompt context

This is a deliberate "good enough for M1" decision. The index will surface relevant context in the majority of cases (same-package, same-module calls). It will miss cross-package type resolution and will have false positives for common names.

---

## Incremental Scoping: The Change Cone

The index must NOT parse the entire repository on every PR. Work is proportional to PR size, not repo size.

**Algorithm:**

```
1. git diff --name-only base_sha..head_sha → changed_files
2. For each file in changed_files:
   a. tree-sitter parse → extract changed symbols (functions, types, methods)
   b. Extract the file's package/module path
3. Find importer files:
   a. Grep the repo for import statements referencing changed packages
   b. This is a text search scoped to import blocks, NOT a full-file grep
4. For each importer file:
   a. tree-sitter parse → extract only functions that reference changed symbols
5. Result: a focused set of (symbol, callers, tests) tuples — the "change cone"
```

The change cone is the unit of work for the planner. Everything outside it is irrelevant to this review.

---

## Import-Based Disambiguation

The original design's name-matching approach has a known problem: common names like `New`, `Close`, `Run`, `Handle` match dozens of irrelevant call sites.

**Fix: two-pass filter.**

1. **Pass 1 — package filter.** Tree-sitter extracts import statements from every file. Only files that import the *package containing the changed function* are candidates. This eliminates the vast majority of false matches.

2. **Pass 2 — symbol match.** Within the filtered file set, match the symbol name against function call expressions in the AST (not raw text grep). Tree-sitter can distinguish `foo.Close()` (a method call on an imported package) from a local `Close()` call.

This is not type-resolved — it can't distinguish `foo.Close()` from `bar.Close()` if both packages are imported. But it's dramatically better than repo-wide name matching, and the remaining ambiguity is bounded by the number of imported packages per file (typically < 20).

---

## Data Model: In-Memory Symbol Table

The index is **ephemeral per pipeline run**. No extra database, no persistent cache, no graph DB, no vector DB.

```go
// SymbolTable is built once per pipeline run and discarded.
type SymbolTable struct {
    // FileSymbols maps file path → symbols defined in that file.
    FileSymbols map[string][]Symbol

    // SymbolRefs maps symbol name → locations where it's referenced.
    // Populated only for changed symbols (not the entire repo).
    SymbolRefs map[string][]Reference

    // TestFiles maps source file → corresponding test file(s).
    // Heuristic: foo.go → foo_test.go in the same package.
    TestFiles map[string][]string

    // ImportGraph maps file path → imported package paths.
    // Used for the two-pass disambiguation filter.
    ImportGraph map[string][]string
}

type Symbol struct {
    Name     string
    Kind     string // "func" | "method" | "type" | "interface"
    FilePath string
    StartLine int
    EndLine   int
}

type Reference struct {
    FilePath  string
    Line      int
    InFunc    string // enclosing function name, if any
}
```

This is a set of hash maps. It fits in memory for any repo that fits on disk. For a 10,000-file Go repo, the symbol table is roughly 50–100 MB — well within a worker's memory budget.

---

## Caching Strategy

**M1: No cache. Rebuild per run.** Tree-sitter parsing is fast (~1–5ms per file). For a PR touching 10 files with 200 importers, the total parse time is ~1 second. This is negligible relative to LLM inference latency (5–30 seconds per task).

**If caching becomes necessary (M2+):** Cache parsed symbol tables keyed by `(file_path, git_blob_sha)`. Git blob SHAs are content-addressed — a changed file has a different blob SHA, so cache invalidation is automatic. No TTL, no manual invalidation, no stale data. Store in a local on-disk cache (bbolt or flat files in a temp directory), not in PostgreSQL.

Do not build this until profiling shows tree-sitter parsing is a bottleneck. It almost certainly won't be.

---

## Graceful Degradation When Approximate

`IsApproximate() bool` on `IndexAdapter` must have concrete downstream effects, not just be a label.

**When the index is approximate:**

1. **Confidence penalty.** The planner applies a confidence ceiling to any finding whose context includes call-graph data from the approximate index. Proposed: multiply the model's confidence score by 0.85 for findings that relied on approximate caller context. This prevents heuristic context noise from producing high-confidence false positives.

2. **Prompt annotation.** The slice sent to the model includes a header: `"Note: caller/callee context was assembled heuristically and may include false matches. Verify relationships before citing them in findings."` This calibrates the model's own uncertainty.

3. **Budget shift toward diff.** When the index is approximate, shift the default token budget from 60/25/15 to 70/17/13 — less call graph (lower signal-to-noise), more diff (the one thing we know is correct). This is a cap adjustment, not a hard reallocation (see slice-budgeting.md).

4. **Summary comment disclosure.** The PR summary comment includes a line: `"Context: semantic index was heuristic for this review. Caller relationships are approximate."` This sets reviewer expectations.

---

## Consequences

**Positive:**
- Working semantic context in M1 — meaningfully better than raw diff alone
- Pure Go binding, no external process, minimal latency
- Parsers available for Go, Python, TypeScript, Rust — covers primary target languages
- Change cone scoping keeps work proportional to PR size
- Import-based disambiguation eliminates the worst false-match problem

**Negative / Accepted trade-offs:**
- No cross-file type resolution in M1. Callers of a changed function are found by package import + name match, not by resolved symbol. Ambiguity remains when multiple imported packages export the same name.
- Interface satisfaction and embedding chains are not tracked
- All findings generated with semantic context should note "approximate context" to calibrate reviewer trust

**M2 scope:**
- Replace heuristic import analysis with gopls (Go) and language server protocol integration for accurate cross-file resolution
- LSP-based index is an explicit `IndexAdapter` implementation — the interface (see `plugin-interfaces.md`) is designed to support pluggable backends
- When `IsApproximate()` returns false, the confidence penalty and prompt annotation are removed automatically

**Rejected alternatives:**
- Full LSP integration for M1: Too much work. gopls integration alone requires subprocess management, JSON-RPC framing, workspace initialization, and handling of incremental sync. Correct for M2.
- Ctags/Universal Ctags: Less accurate than tree-sitter for structured queries; no native Go API
- Graph DB for call graph storage: The call graph is shallow (one hop), ephemeral (per-run), and fits in a hash map. A graph DB adds operational complexity for zero benefit.
- Vector DB for semantic similarity: No embedding-based query pattern exists in M1 or M2. If needed in M3+, `pgvector` (PostgreSQL extension) would be evaluated first.
