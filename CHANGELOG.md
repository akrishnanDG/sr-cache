# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[SemVer](https://semver.org/).

## [Unreleased]

## [0.1.0] — 2026-05-12

Initial public release.

### Added
- Read-through caching proxy for Schema Registry, built in Go.
- Singleflight request coalescing — N concurrent identical misses collapse to one upstream call.
- Per-route cache key + TTL policy:
  - `/schemas/ids/{id}` — cached forever (immutable)
  - `/subjects/{sub}/versions/{N}` (numeric) — cached forever
  - `/subjects/{sub}/versions/latest` — 30s TTL
  - listings (`/subjects`, `/subjects/{sub}/versions`) — 10s TTL
- Cache-key sanitization: only an allowlisted set of query parameters
  participates in cache keys + is forwarded upstream, defending against
  cardinality DoS via arbitrary query strings.
- Write-side query parameter passthrough for `permanent`, `force`, `id`,
  `version` so hard-deletes and explicit-version registrations work as expected.
- Cache invalidation on writes, including `sr:id:*` flush on `?permanent=true`
  hard-deletes.
- Circuit breaker tripped by upstream 429s and transport errors (TCP reset,
  DNS failure, context deadline). HalfOpen probe semantics; auto-recovers
  to Closed on first successful upstream call.
- Optional client-identity middleware: reads a configurable header
  (default `X-Client-CN`), enforces an optional allowlist, emits per-request
  audit log lines, optionally labels Prometheus metrics by CN. Designed
  for use behind a verified Ingress that terminates mTLS.
- Redis / Valkey backend with go-redis/v9; supports username (ACL),
  password, TLS with CA pinning, connection pooling.
- Prometheus `/metrics` endpoint exposing cache hits / misses per route,
  upstream RPS over a 60s window, request-latency histogram, breaker state,
  redis up, and optional per-CN request counter.
- Self-contained admin dashboard at `/admin` — vanilla SVG charts, no
  external CDN dependencies. Polls `/admin/stats` every 5s, pauses on
  Page Visibility hidden.
- Operational endpoints: `/healthz` (liveness), `/readyz` (Redis-aware
  readiness with 3-failure threshold to tolerate transient blips).
- Kubernetes manifests: Deployment, Service, HPA (CPU 70%, 2-20 replicas),
  PodDisruptionBudget, optional Ingress with nginx-ingress mTLS
  annotations + NetworkPolicy, optional Valkey StatefulSet.
- Multi-stage Dockerfile producing a distroless, non-root image.
- Reference clients in Python, Go, and Java exercising real Avro
  produce/consume through the proxy.
- Documentation: README, DEPLOYMENT (5 deployment patterns + sizing),
  RUNBOOK (triage, alerting, day-2 procedures).
