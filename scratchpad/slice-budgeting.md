# Slice Budgeting (Draft)

> **Status:** Draft — numbers are placeholders pending empirical validation.
> Do not codify these as defaults until we have real review runs to measure against.

---

## What Is a Slice?

A `Slice` is the token-budgeted context window assembled for a single `ReviewTask`. It contains:

1. **Diff hunk** — the raw changed lines for the function/symbol being reviewed, plus N lines of surrounding context
2. **Call graph context** — callers and callees of the changed function (from `IndexAdapter`)
3. **Test context** — test functions that exercise the changed function (from `IndexAdapter`)

The slice is constructed by `internal/runtime` before calling `ModelAdapter.Infer`.

---

## Proposed Default Split

Total available = model context window − prompt overhead (~2,000 tokens for system prompt + task framing + output schema).

| Slice Type       | Allocation | Rationale |
|------------------|------------|-----------|
| Diff hunk        | 60%        | The primary signal; reviewer needs the actual change |
| Call graph ctx   | 25%        | Callers reveal how the function is used; critical for logic/security review |
| Test context     | 15%        | Test presence/absence and test quality signals |

**Example for claude-opus-4-6 (200K context):**
- Total available: ~198,000 tokens (after prompt overhead)
- Practical cap: 16,000 tokens per task (to allow ~12 parallel tasks within a single job)
- Diff hunk: 9,600 tokens (~7,200 words)
- Call graph: 4,000 tokens
- Test context: 2,400 tokens

---

## Hard Cap

`MaxSliceTokens` is configured per model and must be respected. If the diff hunk alone exceeds the cap (large function rewrite), truncate from the bottom (least relevant context) and append a `[truncated]` marker.

**Never silently truncate.** The model must know that context is incomplete so it can express lower confidence.

---

## Per-Task-Type Adjustments

Different task types benefit from different allocations:

| Task Type      | Suggested Adjustment |
|----------------|---------------------|
| security       | +10% call graph (attacker entry points matter more than tests) |
| test_coverage  | +20% test context, −20% call graph |
| logic          | Default split |
| style          | Diff hunk only; skip call graph and tests entirely (budget is wasted) |

These are intuitions, not measurements.

---

## Open Questions

### [OPEN] Empirical Validation
The 60/25/15 split is a prior-free guess. Before M1 ships, we need:

1. A dataset of ≥ 100 real PRs with known findings (manually labeled or from historical review data)
2. A replay harness (`internal/eval`) that can run the pipeline with different budget splits
3. A metric: precision/recall of findings vs. labels, plus cost per finding

Until this exists, the split is a knob, not a decision.

### [RESOLVED] Dynamic Budget Allocation
**Resolution:** Dynamic. The budget split is a **cap**, not an allocation.

The slice builder fills each slot in priority order:
1. **Diff hunk** — fill up to cap (60% default, 70% when index is approximate)
2. **Call graph** — fill up to cap (25% default, 17% when approximate) with whatever the index returns. If the index returns fewer tokens than the cap, the unused budget rolls to diff.
3. **Test context** — fill up to cap (15% default, 13% when approximate). Unused budget rolls to diff.

This means a private function with no callers and no tests gets its entire budget for diff context — no tokens wasted on empty slots. The per-task-type adjustments (security: +10% call graph; test_coverage: +20% tests; style: diff only) are expressed as cap overrides, not allocation shifts.

**Implementation:** The slice builder queries the index first (to know how much content is available per slot), then allocates. This is ~10 lines of logic — a loop with a running remainder.

### [OPEN] Multi-Turn vs. Single-Shot
Current design: single inference call per task. The model gets one shot at the slice and returns findings.

Alternative: multi-turn — send the diff hunk first, ask the model what additional context it needs, then send call graph or test context selectively. More expensive (2x model calls) but potentially higher precision for complex cases.

Not in scope for M1, but the `ModelAdapter` interface should not preclude it.

### [OPEN] Token Counting
Token counting must happen before the model call to ensure we don't exceed limits. Options:
1. Use the model provider's tokenizer (accurate, but adds a dependency per model)
2. Rough estimate: 1 token ≈ 4 characters (fast, ~10–15% error margin)
3. tiktoken or equivalent (fast, accurate for GPT models; approximate for Claude)

M1: use character estimate with a 20% safety margin. Revisit if we're hitting context limit errors in practice.
