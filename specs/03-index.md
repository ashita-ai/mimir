# Spec 03: Semantic Index — Tree-sitter

> **Status:** M1 Build Spec
> **Date:** 2026-03-27
> **Package:** `internal/index`
> **Implements:** `pkg/adapter.IndexAdapter`
> **ADR:** [0004-semantic-index-treesitter.md](../adr/0004-semantic-index-treesitter.md)

---

## Responsibilities

1. Parse changed files with tree-sitter to extract symbols
2. Build the change cone: find files that reference changed symbols
3. Construct the in-memory `SymbolTable`
4. Answer `Query` requests with token-budgeted context slices
5. Report `IsApproximate()` status per language

---

## Repo Checkout Strategy

Tree-sitter requires files on disk. The pipeline cannot operate solely on GitHub API responses (which provide diffs and metadata, not the full working tree). Each pipeline run must have a local checkout of the repository at the head SHA.

### M1 Implementation

```go
// checkoutRepo creates a shallow clone of the repo at the given SHA.
// Returns the path to the checkout directory and a cleanup function.
func checkoutRepo(ctx context.Context, repoFullName, headSHA, baseSHA, token string) (string, func(), error) {
    dir, err := os.MkdirTemp("", "mimir-checkout-*")
    if err != nil {
        return "", nil, fmt.Errorf("create temp dir: %w", err)
    }
    cleanup := func() { os.RemoveAll(dir) }

    // Shallow clone with only the commits we need.
    // --filter=blob:none fetches tree structure immediately but defers blob
    // downloads until checkout, reducing clone time for large repos.
    cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, repoFullName)
    cmds := [][]string{
        {"git", "clone", "--filter=blob:none", "--no-checkout", "--single-branch", cloneURL, dir},
        {"git", "-C", dir, "fetch", "origin", headSHA, "--depth=1"},
        {"git", "-C", dir, "checkout", headSHA},
    }
    for _, args := range cmds {
        cmd := exec.CommandContext(ctx, args[0], args[1:]...)
        if out, err := cmd.CombinedOutput(); err != nil {
            cleanup()
            return "", nil, fmt.Errorf("git %s: %w: %s", args[1], err, out)
        }
    }

    return dir, cleanup, nil
}
```

### Lifecycle

1. **Created** at the start of the River job handler, before `BuildSymbolTable`.
2. **Used** by `BuildSymbolTable` and `Query` (via the `repoPath` parameter on `IndexAdapter`).
3. **Used** by `addressed_in_next_commit` (re-parsing files at the new head SHA).
4. **Cleaned up** via the `cleanup` function after `CompletePipelineRun`, in a `defer`. The checkout directory is ephemeral — it does not survive process restarts. If River re-enqueues the job, a fresh checkout is created.

### Security

The clone URL uses the installation token (or PAT), not embedded credentials. The temp directory is created with restrictive permissions (`0700` via `os.MkdirTemp` defaults). The cleanup function runs even on panic via `defer`.

### Disk Budget

A blobless clone of a typical 10k-file Go repo is ~50–100 MB on disk. With 5 concurrent workers, peak disk usage is ~500 MB. For repos exceeding 1 GB, M2 can add sparse checkout (clone only changed files + their importers). M1 uses full checkout for simplicity.

---

## Language Support Matrix

| Language | Grammar | Import Analysis | Test Heuristic | `IsApproximate()` |
|----------|---------|-----------------|----------------|-------------------|
| Go | `smacker/go-tree-sitter` | Full: `import` blocks → package path matching | `_test.go` in same package | `false` |
| Python | tree-sitter-python | Approximate: `import`/`from` statements, no `__init__.py` re-export tracking | `test_*.py`, `*_test.py` | `true` |
| TypeScript | tree-sitter-typescript | Approximate: `import`/`require`, no barrel export resolution | `*.test.ts`, `*.spec.ts`, `*.test.tsx`, `*.spec.tsx` | `true` |
| PHP | tree-sitter-php | Approximate: `use` statements, PSR-4 namespace conventions | `*Test.php` in same namespace | `true` |
| YAML/Helm | None | None | None | N/A (skip index) |

For files with no tree-sitter grammar (YAML, Markdown, config files, etc.), the index returns an empty result. The runtime uses the full token budget for the raw diff.

---

## Change Cone Algorithm

Input: `base_sha`, `head_sha`, repo path on disk.

```
Step 1: Identify changed files
    git diff --name-only {base_sha}..{head_sha} → changed_files[]

Step 2: Parse changed files, extract symbols
    For each file in changed_files:
        If no grammar for this file extension → skip
        tree-sitter parse → AST
        Extract: function definitions, method definitions, type definitions, interface definitions
        Extract: import statements → populate ImportGraph[file]
        Diff the AST against the base version to identify *changed* symbols
            (symbols whose byte range overlaps a diff hunk)
        Store in SymbolTable.FileSymbols[file]

Step 3: Find importer files (package filter — Pass 1)
    For each changed file:
        Determine its package path (language-specific)
        Search the repo for files that import this package:
            Go:    grep for the import path string in import blocks
            Python: grep for "from {module}" or "import {module}"
            TS:    grep for "from './{relative_path}'" or require
            PHP:   grep for "use {namespace}"
        Scope: search only in import/use blocks, not full-file grep
        Result: importer_files[] (candidate files that *might* reference changed symbols)

Step 4: Parse importer files, extract references (Symbol match — Pass 2)
    For each importer_file:
        tree-sitter parse → AST
        For each changed symbol:
            Search AST for call expressions or type references matching:
                - Qualified: {package_alias}.{symbol_name}
                - Unqualified: {symbol_name} (only if file imports the right package)
            If found: add to SymbolTable.SymbolRefs[symbol_name]

Step 5: Identify test files
    For each changed file:
        Apply language-specific heuristic:
            Go:    same_dir/{base}_test.go
            Python: same_dir/test_{base}.py or same_dir/{base}_test.py
            TS:    same_dir/{base}.test.ts or same_dir/{base}.spec.ts
            PHP:   same_dir/{base}Test.php or tests/{base}Test.php
        If test file exists: SymbolTable.TestFiles[file] = [test_file]

Step 6: Return SymbolTable
```

### Diff-Based Symbol Change Detection (Step 2 Detail)

Not every symbol in a changed file was actually changed. To identify which symbols overlap with diff hunks:

```go
// changedRanges extracts line ranges from unified diff hunks for a file.
func changedRanges(diff string, filePath string) []LineRange {
    // Parse @@ -a,b +c,d @@ headers for this file
    // Return []LineRange{{Start: c, End: c+d}} for each hunk
}

// symbolOverlapsDiff returns true if the symbol's line range
// intersects any changed line range.
func symbolOverlapsDiff(sym Symbol, ranges []LineRange) bool {
    for _, r := range ranges {
        if sym.StartLine <= r.End && sym.EndLine >= r.Start {
            return true
        }
    }
    return false
}
```

Only symbols that overlap a diff hunk become `ReviewTask` candidates. Unchanged symbols in changed files are indexed (they populate the `SymbolTable` for call-graph lookups) but not reviewed.

---

## Tree-sitter Query Patterns

### Go

```scheme
;; Function definitions
(function_declaration
  name: (identifier) @name) @func

;; Method definitions
(method_declaration
  name: (field_identifier) @name
  receiver: (parameter_list
    (parameter_declaration
      type: (_) @receiver_type))) @method

;; Type definitions
(type_declaration
  (type_spec
    name: (type_identifier) @name)) @type

;; Import statements
(import_declaration
  (import_spec
    path: (interpreted_string_literal) @path)) @import

;; Function calls (for reference detection)
(call_expression
  function: (selector_expression
    operand: (identifier) @package
    field: (field_identifier) @func_name)) @call
```

### Python

```scheme
;; Function definitions
(function_definition
  name: (identifier) @name) @func

;; Class definitions
(class_definition
  name: (identifier) @name) @type

;; Import statements
(import_from_statement
  module_name: (dotted_name) @module) @import

(import_statement
  name: (dotted_name) @module) @import

;; Function calls
(call
  function: (attribute
    object: (identifier) @module
    attribute: (identifier) @func_name)) @call
```

### TypeScript

```scheme
;; Function declarations
(function_declaration
  name: (identifier) @name) @func

;; Arrow functions assigned to const/let
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: (arrow_function))) @func

;; Class declarations
(class_declaration
  name: (type_identifier) @name) @type

;; Interface declarations
(interface_declaration
  name: (type_identifier) @name) @type

;; Import statements
(import_statement
  source: (string) @path) @import

;; Require calls
(call_expression
  function: (identifier) @_req (#eq? @_req "require")
  arguments: (arguments (string) @path)) @import
```

### PHP

```scheme
;; Function definitions
(function_definition
  name: (name) @name) @func

;; Method definitions
(method_declaration
  name: (name) @name) @method

;; Class definitions
(class_declaration
  name: (name) @name) @type

;; Interface definitions
(interface_declaration
  name: (name) @name) @type

;; Use statements
(use_declaration
  (use_list
    (use_clause
      (qualified_name) @path))) @import

;; Namespace-qualified calls
(member_call_expression
  object: (variable_name) @obj
  name: (name) @func_name) @call
```

---

## Import-Based Disambiguation

The two-pass filter from ADR-0004, made concrete.

### Example (Go)

Changed function: `auth.ValidateToken` in package `github.com/ashita-ai/mimir/internal/auth`.

**Pass 1 — Package filter:**
Grep the repo for files containing `"github.com/ashita-ai/mimir/internal/auth"` in import blocks. Result: 12 files.

**Pass 2 — Symbol match:**
For each of the 12 files, tree-sitter parse and search for `auth.ValidateToken` call expressions. The query matches `(selector_expression operand:(identifier) @pkg field:(field_identifier) @fn)` where `@pkg = "auth"` and `@fn = "ValidateToken"`. Result: 4 files contain calls.

These 4 files are the callers. Their relevant functions (the ones containing the call) are added to `SymbolRefs["ValidateToken"]`.

### Known Limitation

If two imported packages alias to the same name (e.g., `auth "pkg/a"` and `auth "pkg/b"`), Pass 2 cannot distinguish them. Both are included as potential callers. This is the "approximate" in `IsApproximate()`. The confidence penalty and prompt annotation handle this downstream.

---

## IsApproximate() Downstream Effects

When `IsApproximate()` returns `true` for the current pipeline run:

1. **Confidence penalty.** The policy layer multiplies each finding's `ConfidenceScore` by 0.85 if the finding's slice included call-graph or test context from the approximate index. Findings based on diff-only slices are not penalized.

2. **Prompt annotation.** The slice sent to the model includes:
   ```
   Note: caller/callee context was assembled heuristically and may include
   false matches. Verify relationships before citing them in findings.
   ```

3. **Budget shift.** Default token budget shifts from 60/25/15 to 70/17/13 (less call graph, more diff). See `specs/05-runtime.md`.

4. **Summary comment disclosure.** The summary comment footer includes:
   ```
   Context: semantic index was heuristic for this review. Caller relationships are approximate.
   ```

---

## Performance Budget

- Tree-sitter parse: ~1–5ms per file. For a PR touching 10 files with 200 importers: ~1 second total.
- Memory: ~50–100 MB for a 10,000-file repo's symbol table. Well within worker memory.
- The index is rebuilt per pipeline run. No cache in M1.

If profiling shows parsing is a bottleneck (unlikely — LLM latency dominates), M2 can add a cache keyed by `(file_path, git_blob_sha)`. See ADR-0004 caching strategy section.

---

## Files Without Grammar Support

For files the index can't parse (YAML, Markdown, JSON, Dockerfile, Makefile, `.sh`, etc.):

1. `BuildSymbolTable` skips the file — no symbols extracted, no references traced.
2. The planner still creates a single `ReviewTask` per changed file (file-level, not symbol-level).
3. The runtime builds a diff-only slice (100% of budget on raw diff, no call graph or test context).
4. The model reviews the raw diff without semantic context.

This ensures Helm charts, Dockerfiles, and CI configs still get LLM review — just without the semantic enrichment that tree-sitter enables.
