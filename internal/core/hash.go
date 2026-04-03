package core

import (
	"crypto/sha256"
	"encoding/hex"
)

// ComputeLocationHash produces the deterministic location fingerprint for
// finding dedup (ADR-0002). The hash contains no LLM output — only the
// repo, file path, symbol name, and review category — so it is stable
// across model changes and prompt revisions.
//
// Null bytes separate fields to prevent collisions between values like
// ("ab", "cd") and ("a", "bcd").
func ComputeLocationHash(repoFullName, filePath, symbol string, category Category) string {
	h := sha256.New()
	h.Write([]byte(repoFullName))
	h.Write([]byte{0})
	h.Write([]byte(filePath))
	h.Write([]byte{0})
	h.Write([]byte(symbol))
	h.Write([]byte{0})
	h.Write([]byte(category))
	return hex.EncodeToString(h.Sum(nil))
}

// NewTokenBudget computes per-section token caps for a given total budget,
// task type, and index precision. The returned caps follow the allocation
// strategy in slice-budgeting.md:
//
//   - Default split: 60% diff, 25% call graph, 15% tests
//   - Approximate index: 70% diff, 17% call graph, 13% tests
//   - Per-task-type overrides adjust the default before applying
//
// These are caps, not allocations. The runtime slice builder fills in
// priority order (diff → call graph → tests) and rolls unused budget
// from later slots into diff.
func NewTokenBudget(totalTokens int, taskType TaskType, approximate bool) TokenBudget {
	// Base percentages (expressed as basis points for integer math).
	var diffBP, callGraphBP, testsBP int

	if approximate {
		// Approximate index: shift budget toward diff, away from call graph / tests.
		diffBP, callGraphBP, testsBP = 7000, 1700, 1300
	} else {
		diffBP, callGraphBP, testsBP = 6000, 2500, 1500
	}

	// Per-task-type adjustments (slice-budgeting.md §Per-Task-Type Adjustments).
	switch taskType {
	case TaskTypeSecurity:
		// +10% call graph (from diff) — entry points matter more.
		diffBP -= 1000
		callGraphBP += 1000
	case TaskTypeTestCoverage:
		// +20% tests, −20% call graph.
		callGraphBP -= 2000
		testsBP += 2000
	case TaskTypeStyle:
		// Diff only — call graph and tests are wasted budget.
		diffBP = 10000
		callGraphBP = 0
		testsBP = 0
	case TaskTypeLogic:
		// Default split, no adjustment.
	}

	return TokenBudget{
		DiffHunk:  totalTokens * diffBP / 10000,
		CallGraph: totalTokens * callGraphBP / 10000,
		Tests:     totalTokens * testsBP / 10000,
	}
}

// Total returns the sum of all section caps.
func (b TokenBudget) Total() int {
	return b.DiffHunk + b.CallGraph + b.Tests
}
