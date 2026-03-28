// Package adapter defines the plugin interfaces that make Mimir's pipeline
// extensible. Each interface represents one boundary in the review pipeline:
//
//   - ProviderAdapter: code hosting platform (GitHub, GitLab)
//   - ModelAdapter:    LLM inference provider (Anthropic, OpenAI)
//   - StaticToolAdapter: static analysis tools (semgrep, golangci-lint)
//   - IndexAdapter:    semantic repo map backend (tree-sitter, LSP)
//   - PolicyAdapter:   finding gating and triage
//   - StoreAdapter:    persistence (PostgreSQL)
//
// Implementations live in internal/. This package exports only interfaces
// and the request/response types they reference.
//
// NOTE: These interfaces reference types from internal/core, which is
// importable within this module but not by external consumers. When M2
// introduces third-party adapter implementations, core types will be
// promoted to pkg/core.
package adapter
