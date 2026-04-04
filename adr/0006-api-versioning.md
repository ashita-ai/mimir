# ADR-0006: API Versioning — Path-Prefixed Routes

> **Status:** Accepted
> **Date:** 2026-04-04

---

## Context

Mimir exposes an HTTP API for webhook reception and health checks. As of M1, the API surface is small (`/healthz`, `/webhooks/github`), but breaking changes are inevitable as the system grows — new endpoints, payload schema changes, authentication changes, etc. Retrofitting versioning onto an established API is painful: it requires coordinating client migration, maintaining backward-compatible routes, and risks breaking existing webhook configurations.

---

## Decision

**All HTTP routes are served under a `/v1` path prefix.**

Routes become:
- `GET /v1/healthz`
- `POST /v1/webhooks/github`

Versioning is implemented via `chi.Router.Route("/v1", ...)`, which groups all current routes under the prefix with zero per-request overhead.

---

## Consequences

**Positive:**
- Breaking changes in the future get a new prefix (`/v2`) while existing clients continue on `/v1`
- GitHub webhook URLs include the version, making it visible which API contract an installation targets
- Load balancers and reverse proxies can route by version prefix if needed
- Near-zero implementation cost when done at project inception

**Negative / Accepted trade-offs:**
- GitHub App webhook URLs configured in repository settings must include `/v1`. If we ever deprecate v1, reconfiguring webhooks across installations requires coordination. Acceptable: webhook URL changes are infrequent and well-understood operationally.
- Slightly longer URLs. Negligible.

**Rejected alternatives:**
- Header-based versioning (`Accept-Version`, `X-API-Version`): Harder to inspect in logs and load balancer configs. Overkill for this API surface. Would require custom middleware to parse and route.
- Query parameter versioning (`?v=1`): Non-standard, complicates caching, not idiomatic for REST APIs.
- No versioning (add later when needed): Retrofitting is strictly harder than starting with it. The cost now is one `r.Route()` call; the cost later is a migration.
