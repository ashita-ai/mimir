# ADR-0004: Semantic Index — Tree-sitter (M1)

**Status:** Accepted
**Date:** 2026-03-18

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

## Consequences

**Positive:**
- Working semantic context in M1 — meaningfully better than raw diff alone
- Pure Go binding, no external process, minimal latency
- Parsers available for Go, Python, TypeScript, Rust — covers primary target languages

**Negative / Accepted trade-offs:**
- No cross-file type resolution in M1. Callers of a changed function are found by name match, not by resolved symbol. Common names (e.g., `New`, `Close`) will have false positives.
- Interface satisfaction and embedding chains are not tracked
- All findings generated with semantic context should note "approximate context" to calibrate reviewer trust

**M2 scope:**
- Replace heuristic import analysis with gopls (Go) and language server protocol integration for accurate cross-file resolution
- LSP-based index is an explicit `IndexAdapter` implementation — the interface (see `scratchpad/plugin-interfaces.md`) is designed to support pluggable backends

**Rejected alternatives:**
- Full LSP integration for M1: Too much work. gopls integration alone requires subprocess management, JSON-RPC framing, workspace initialization, and handling of incremental sync. Correct for M2.
- Ctags/Universal Ctags: Less accurate than tree-sitter for structured queries; no native Go API
