package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeLocationHash(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		h1 := ComputeLocationHash("ashita-ai/mimir", "internal/core/types.go", "FindingValidate", CategorySecurity)
		h2 := ComputeLocationHash("ashita-ai/mimir", "internal/core/types.go", "FindingValidate", CategorySecurity)
		assert.Equal(t, h1, h2)
	})

	t.Run("hex encoded sha256", func(t *testing.T) {
		h := ComputeLocationHash("repo", "file.go", "Fn", CategoryLogic)
		assert.Len(t, h, 64, "sha256 hex digest is 64 chars")
	})

	t.Run("different inputs produce different hashes", func(t *testing.T) {
		base := ComputeLocationHash("repo", "file.go", "Fn", CategoryLogic)

		assert.NotEqual(t, base, ComputeLocationHash("other", "file.go", "Fn", CategoryLogic), "different repo")
		assert.NotEqual(t, base, ComputeLocationHash("repo", "other.go", "Fn", CategoryLogic), "different file")
		assert.NotEqual(t, base, ComputeLocationHash("repo", "file.go", "Other", CategoryLogic), "different symbol")
		assert.NotEqual(t, base, ComputeLocationHash("repo", "file.go", "Fn", CategorySecurity), "different category")
	})

	t.Run("null byte separator prevents collisions", func(t *testing.T) {
		// Without a separator, ("ab","cd") and ("a","bcd") would hash identically.
		h1 := ComputeLocationHash("ab", "cd", "e", CategoryLogic)
		h2 := ComputeLocationHash("a", "bcd", "e", CategoryLogic)
		assert.NotEqual(t, h1, h2)
	})

	t.Run("empty symbol is valid", func(t *testing.T) {
		h := ComputeLocationHash("repo", "file.go", "", CategoryStyle)
		assert.Len(t, h, 64)
	})
}

func TestNewTokenBudget(t *testing.T) {
	const total = 16000

	t.Run("default logic split", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeLogic, false)
		assert.Equal(t, 9600, b.DiffHunk)
		assert.Equal(t, 4000, b.CallGraph)
		assert.Equal(t, 2400, b.Tests)
		assert.Equal(t, total, b.Total())
	})

	t.Run("approximate index shifts toward diff", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeLogic, true)
		assert.Equal(t, 11200, b.DiffHunk)
		assert.Equal(t, 2720, b.CallGraph)
		assert.Equal(t, 2080, b.Tests)
		assert.Equal(t, total, b.Total())
	})

	t.Run("security adds call graph budget", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeSecurity, false)
		assert.Equal(t, 8000, b.DiffHunk)  // 50%
		assert.Equal(t, 5600, b.CallGraph)  // 35%
		assert.Equal(t, 2400, b.Tests)      // 15%
		assert.Equal(t, total, b.Total())
	})

	t.Run("test_coverage adds test budget", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeTestCoverage, false)
		assert.Equal(t, 9600, b.DiffHunk)  // 60%
		assert.Equal(t, 800, b.CallGraph)   // 5%
		assert.Equal(t, 5600, b.Tests)      // 35%
		assert.Equal(t, total, b.Total())
	})

	t.Run("style is diff only", func(t *testing.T) {
		b := NewTokenBudget(total, TaskTypeStyle, false)
		assert.Equal(t, total, b.DiffHunk)
		assert.Equal(t, 0, b.CallGraph)
		assert.Equal(t, 0, b.Tests)
		assert.Equal(t, total, b.Total())
	})

	t.Run("zero budget produces zero caps", func(t *testing.T) {
		b := NewTokenBudget(0, TaskTypeLogic, false)
		assert.Equal(t, 0, b.Total())
	})
}

func TestTokenBudgetTotal(t *testing.T) {
	b := TokenBudget{DiffHunk: 100, CallGraph: 50, Tests: 25}
	require.Equal(t, 175, b.Total())
}
