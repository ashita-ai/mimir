# Spec 08: Eval — Metrics & Feedback

> **Status:** M1 Build Spec
> **Date:** 2026-03-27
> **Package:** `internal/eval`

---

## M1 Scope

Eval in M1 is minimal: collect signal, provide basic queries. The replay harness and offline evaluation are M2.

| Component | M1 | M2 |
|-----------|-----|-----|
| Reaction-based feedback collection | Yes | — |
| SQL-based scorecard queries | Yes | — |
| Data capture for future replay | Yes | — |
| Replay harness (re-run pipeline on captured data) | No | Yes |
| Offline eval against labeled dataset | No | Yes |
| Prometheus/OpenTelemetry export | No | Yes |

---

## Reaction-Based Feedback

Covered in `specs/02-ingest.md` (reaction polling). The eval package consumes `finding_events` rows written by the ingest poller.

**Signal mapping:**

| Reaction | Interpretation | `event_type` |
|----------|---------------|-------------|
| :+1: | Finding was helpful / true positive | `thumbs_up` |
| :-1: | Finding was unhelpful / false positive | `thumbs_down` |

---

## Scorecard Queries

Basic eval metrics, computed via SQL against existing tables. No separate metrics infrastructure.

### Precision Proxy (False Positive Rate)

```sql
-- Findings with thumbs_down / total reacted findings
SELECT
    COUNT(*) FILTER (WHERE e.event_type = 'thumbs_down') AS false_positives,
    COUNT(*) FILTER (WHERE e.event_type IN ('thumbs_up', 'thumbs_down')) AS total_reacted,
    CASE
        WHEN COUNT(*) FILTER (WHERE e.event_type IN ('thumbs_up', 'thumbs_down')) > 0
        THEN ROUND(
            COUNT(*) FILTER (WHERE e.event_type = 'thumbs_down')::NUMERIC /
            COUNT(*) FILTER (WHERE e.event_type IN ('thumbs_up', 'thumbs_down'))::NUMERIC, 3)
        ELSE NULL
    END AS false_positive_rate
FROM findings f
JOIN finding_events e ON e.finding_id = f.id
WHERE f.created_at > now() - interval '7 days';
```

**Target:** < 20% false positive rate. This is a weak label (no reaction ≠ true positive), but it's free signal.

### Task Success Rate

```sql
SELECT
    COUNT(*) AS total_tasks,
    COUNT(*) FILTER (WHERE status = 'completed') AS completed,
    COUNT(*) FILTER (WHERE status = 'failed') AS failed,
    ROUND(COUNT(*) FILTER (WHERE status = 'failed')::NUMERIC / COUNT(*)::NUMERIC, 3) AS failure_rate
FROM review_tasks
WHERE created_at > now() - interval '7 days';
```

**Target:** < 5% task failure rate (transient LLM errors, not systemic issues).

### Findings Per PR

```sql
SELECT
    pr.repo_full_name,
    pr.pr_number,
    COUNT(f.id) AS total_findings,
    COUNT(f.id) FILTER (WHERE f.confidence_tier = 'high') AS high_confidence,
    COUNT(f.id) FILTER (WHERE f.confidence_tier = 'medium') AS medium_confidence,
    COUNT(f.id) FILTER (WHERE f.posted_at IS NOT NULL) AS posted
FROM pull_requests pr
LEFT JOIN findings f ON f.pull_request_id = pr.id
WHERE pr.created_at > now() - interval '7 days'
GROUP BY pr.id, pr.repo_full_name, pr.pr_number
ORDER BY total_findings DESC;
```

### Confidence Tier Distribution

```sql
SELECT
    confidence_tier,
    COUNT(*) AS count,
    ROUND(AVG(confidence_score), 3) AS avg_score,
    ROUND(STDDEV(confidence_score), 3) AS stddev_score
FROM findings
WHERE created_at > now() - interval '7 days'
GROUP BY confidence_tier;
```

### Model Usage & Cost Proxy

```sql
SELECT
    model_id,
    COUNT(*) AS total_calls,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    ROUND(AVG(prompt_tokens + COALESCE(completion_tokens, 0))) AS avg_tokens_per_call
FROM findings
WHERE created_at > now() - interval '7 days'
GROUP BY model_id;
```

### Addressed Status Tracking

```sql
SELECT
    addressed_status,
    COUNT(*) AS count
FROM findings
WHERE created_at > now() - interval '7 days'
GROUP BY addressed_status;
```

---

## Data Capture for Replay (M2 Prep)

During M1 pipeline runs, capture enough data to replay reviews offline later. Store in `findings.metadata` JSONB:

```json
{
  "slice": {
    "diff_hunk_tokens": 9200,
    "call_graph_tokens": 3800,
    "test_context_tokens": 2100,
    "approximate": true,
    "truncated": false
  },
  "model_request": {
    "model_id": "claude-opus-4-6",
    "prompt_hash": "sha256:abc123...",
    "temperature": 0,
    "max_tokens": 4096
  },
  "timing": {
    "slice_build_ms": 120,
    "inference_ms": 8500,
    "total_ms": 8700
  }
}
```

The full prompt text is NOT stored (too large). Instead, store a `prompt_hash` so that replays with the same prompt template version produce the same hash, confirming reproducibility. The slice content can be reconstructed from the git SHAs + symbol table.

---

## M1 Eval Workflow

No CLI command or dashboard. The engineer runs the scorecard queries directly against PostgreSQL:

```bash
# Connect to the DB
docker compose exec postgres psql -U mimir -d mimir

# Run scorecard queries from above
```

M2 adds `mimir eval` subcommand and a structured output format.
