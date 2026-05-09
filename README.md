# sr-cache — Schema Registry Caching Proxy

A high-performance, auto-scalable proxy for Confluent Cloud Schema Registry.
Drops upstream RPS by orders of magnitude using Redis read-through caching with
singleflight request coalescing. Designed to absorb thousands of inbound RPS
while staying well under the 150 RPS Cloud SR limit.

```
┌──────────┐        ┌────────────┐        ┌────────────────────┐
│ Clients  │ ─────▶ │  sr-cache  │ ─────▶ │  Confluent Cloud SR│
│ (Kafka,  │ ◀───── │   (Go)     │ ◀───── │  (≤150 RPS budget) │
│  apps)   │        └────┬───────┘        └────────────────────┘
└──────────┘             │
                         ▼
                    ┌──────────┐
                    │  Valkey  │  AOF persistence
                    └──────────┘
```

## What it does

- **Reads** — checks Redis first; on miss, fetches once from SR and caches.
- **Writes** — forwards POST/PUT/DELETE without caching, then opportunistically
  invalidates affected keys (e.g. `/subjects/{sub}/...` POST evicts
  `sr:latest:{sub}:*` and `sr:subjects:*`).
- **Singleflight** — N concurrent identical misses produce 1 upstream call.
- **Immutable forever** — `/schemas/ids/{id}` and `/subjects/{sub}/versions/{N}`
  cache with no TTL; restarting a pod doesn't cold-start the cache (assumes
  Redis with AOF).
- **Volatile briefly** — `/subjects/{sub}/versions/latest` caches with a short
  TTL (default 30s), `/subjects` listings 10s.
- **Circuit breaker** — trips on upstream 429s, serves cached values during the
  cooldown, returns 503 + `Retry-After` for unservable misses.
- **Observable** — Prometheus `/metrics`, embedded admin dashboard at `/admin`.

## Documentation

- **[DEPLOYMENT.md](DEPLOYMENT.md)** — deployment modes (sidecar, ClusterIP,
  service mesh, Ingress + mTLS), cache layer choices, sizing reference,
  pre-deployment checklist.
- **[RUNBOOK.md](RUNBOOK.md)** — operational procedures (cred rotation,
  cache poisoning recovery, breaker stuck, Redis OOM, scaling triggers,
  alerting recommendations).
- This README — quickstart, configuration reference, the mTLS-via-Ingress
  client identity feature, metrics, design notes.

## Deployment modes at a glance

| Pattern | Where the proxy is reachable from | TLS / auth | Best for |
|---|---|---|---|
| **A · Sidecar** | One app pod, via `localhost` | None | One workload that owns its cache |
| **B · ClusterIP** | Other pods in the same cluster | None (NetworkPolicy is the boundary) | Most internal deployments — *the default* |
| **C · Service mesh** | Mesh-encrypted pod-to-pod | mTLS at the mesh | Existing Istio/Linkerd shops |
| **D · Ingress + mTLS** | Outside the cluster, with client cert | mTLS at the Ingress; CN forwarded to proxy | External clients with mTLS requirements |
| **E · Ingress + JWT/API gateway** | Outside the cluster, with bearer token | JWT/OAuth at the Ingress | OIDC/SaaS-style auth |

See **[DEPLOYMENT.md](DEPLOYMENT.md)** for the decision tree, manifest
snippets, and gotchas for each pattern.

## Quick start (local)

Requires Go 1.25+ and a Valkey (or Redis) instance.

```bash
docker run -d --rm --name sr-cache-valkey -p 6379:6379 valkey/valkey:8-alpine

cp .env.example .env
$EDITOR .env   # set UPSTREAM_URL, UPSTREAM_API_KEY, UPSTREAM_API_SECRET

go run .
```

> **Why Valkey?** Open-source (BSD-3, OSI-approved), drop-in compatible with
> Redis 7.2, backed by the Linux Foundation + AWS/Google. `go-redis/v9`
> speaks Valkey natively — no code changes. If you must use Redis CE
> (SSPL/RSALv2 source-available) or Redis 8 (AGPL), `redis:7-alpine` and
> `redis:8-alpine` work as drop-in substitutes.

Then:

```bash
curl http://localhost:8080/subjects               # first call: miss → upstream
curl http://localhost:8080/subjects               # subsequent: hit (sub-ms)
open http://localhost:8080/admin                  # live dashboard
curl http://localhost:8080/metrics                # Prometheus scrape
```

## Configuration (env vars)

| Variable | Default | Notes |
|---|---|---|
| `UPSTREAM_URL` | required | e.g. `https://psrc-xxxxxxx.region.cloud.confluent.cloud` |
| `UPSTREAM_API_KEY` / `UPSTREAM_API_SECRET` | required | Schema Registry API key + secret |
| `UPSTREAM_TIMEOUT` | `10s` | per-request timeout to upstream |
| `REDIS_ADDR` | `localhost:6379` | |
| `REDIS_PASSWORD` | "" | |
| `REDIS_DB` | `0` | |
| `REDIS_POOL_SIZE` | `50` | |
| `LATEST_TTL` | `30s` | TTL for `/versions/latest` |
| `LIST_TTL` | `10s` | TTL for listings & config/mode |
| `LISTEN_ADDR` | `:8080` | |
| `BREAKER_FAIL_THRESHOLD` | `3` | consecutive 429s to open breaker |
| `BREAKER_OPEN_DURATION` | `10s` | cooldown before half-open probe |

`.env` (gitignored) is loaded automatically; values from the real environment
take precedence.

## Endpoints

### Proxied (cached GETs)

| Path | TTL | Key |
|---|---|---|
| `/schemas/ids/{id}` | ∞ | `sr:id:{id}` |
| `/schemas/ids/{id}/schema` | ∞ | `sr:id:{id}:raw` |
| `/subjects` | 10s | `sr:subjects:{query}` |
| `/subjects/{sub}/versions` | 10s | `sr:subject:{sub}:versions:{query}` |
| `/subjects/{sub}/versions/{N}` (numeric) | ∞ | `sr:subject:{sub}:v:{N}:{query}` |
| `/subjects/{sub}/versions/latest` | 30s | `sr:latest:{sub}:{query}` |
| `/config`, `/config/{sub}` | 10s | `sr:config:*` |
| `/mode`, `/mode/{sub}` | 10s | `sr:mode:*` |

Anything else (POST/PUT/DELETE, unknown reads) is forwarded as-is. On 2xx writes,
related cache keys are invalidated.

### Operational

- `GET /metrics` — Prometheus scrape
- `GET /admin` — embedded HTML dashboard (Tailwind + Chart.js)
- `GET /admin/stats` — JSON stats consumed by the dashboard (5s polling)
- `GET /healthz` — process liveness
- `GET /readyz` — Redis-ready check (returns 503 if Redis is down)

## Client identity (mTLS via an Ingress)

The proxy itself does not terminate TLS — that's a job for an Ingress
(nginx-ingress, Envoy/Istio Gateway, AWS ALB). The Ingress verifies the
client's mTLS certificate, extracts the CN, and forwards it to sr-cache as a
configurable HTTP header.

```
Client (with cert) ──mTLS──▶  Ingress  ──HTTP + X-Client-CN──▶  sr-cache  ──HTTPS+Basic──▶  Cloud SR
                              └─ verifies cert against your CA
                              └─ extracts CN, forwards as header
```

Then sr-cache:

- Reads the configured header (`CLIENT_CN_HEADER`, default `X-Client-CN`)
- Optionally rejects requests without it (`CLIENT_CN_REQUIRED=true` → 401)
- Optionally enforces a CN allowlist (`CLIENT_CN_ALLOWLIST=app-foo,billing-svc` → 403 for unlisted)
- Emits a structured audit log line per request (`AUDIT_LOG=true`, default on)
- Optionally labels Prometheus metrics with the CN (`LABEL_METRICS_BY_CN=true`)

Operational endpoints (`/healthz`, `/readyz`, `/metrics`, `/admin/*`) are never
gated on identity — k8s probes and Prometheus scrapers don't need a header.

### Trust model

The header is **hearsay**. Any caller with network reach to sr-cache could
spoof it. For the model to be safe, the proxy must be reachable *only* through
the verified Ingress — typically enforced via:

- `Service` type `ClusterIP` (no public LB)
- A `NetworkPolicy` allowing inbound traffic only from the Ingress controller pods
- Both shown in `k8s/ingress.yaml`

If you bind sr-cache to a public address with the middleware enabled,
the allowlist is meaningless — anyone can set `X-Client-CN: admin`.

### Audit log shape

```
time=...  level=INFO  msg=request  cn=app-foo  method=GET  path=/schemas/ids/123  status=200  outcome=hit  latency_ms=2
time=...  level=INFO  msg=request  cn=intruder method=GET  path=/subjects        status=403  outcome=auth_denied  latency_ms=0
time=...  level=INFO  msg=request  cn=anonymous method=GET path=/subjects        status=401  outcome=auth_missing latency_ms=0
```

Ship to your SIEM via stdout → fluentbit/datadog/splunk.

### Per-CN metrics

When `LABEL_METRICS_BY_CN=true`:

```
requests_by_client_cn_total{cn="app-foo",     outcome="hit"}          1280
requests_by_client_cn_total{cn="app-foo",     outcome="miss"}            8
requests_by_client_cn_total{cn="billing-svc", outcome="hit"}            47
requests_by_client_cn_total{cn="intruder",    outcome="auth_denied"}    12
```

⚠️ Only enable when your CN cardinality is bounded (<1000 distinct values).
Otherwise it'll blow up your Prometheus.

### Defaults

Out-of-the-box (no env vars set), the middleware is permissive:

- Header: `X-Client-CN`
- Required: `false` — anonymous traffic allowed (`cn=anonymous` in logs)
- Allowlist: empty — every CN allowed
- AuditLog: `true` — audit lines emitted to stdout
- LabelMetricsByCN: `false`

This is the right default for trusted-network deployments (sidecar, internal
ClusterIP, mesh-encrypted). Tighten the policy when adding mTLS at the edge.

## Metrics

```
cache_hits_total{route="..."}        # cache hits per route group
cache_misses_total{route="..."}      # cache misses per route group
upstream_requests_total{status="..."}# upstream calls by HTTP status
upstream_rps                         # rolling 60s upstream RPS
request_latency_seconds              # histogram of inbound latency
breaker_state                        # 0=closed, 1=open, 2=half-open
redis_up                             # 1 if last redis ping succeeded
```

## Testing

```bash
go test ./...
```

Unit tests cover singleflight dedup (N=200 concurrent misses → ≤5 upstream
calls), breaker state transitions (closed → open → half-open → closed),
immutable-vs-latest TTL routing, and write-passthrough behavior.

## Load test result (single laptop process)

```
ab -n 5000 -c 100 http://localhost:8080/subjects
→ 985 RPS, 0 failures, hit ratio 99%, 4 upstream calls total
```

Per-request latency for cached responses: ~1ms.

## Docker

```bash
docker build -t sr-cache:latest .
docker run --rm -p 8080:8080 \
  -e UPSTREAM_URL=... -e UPSTREAM_API_KEY=... -e UPSTREAM_API_SECRET=... \
  -e REDIS_ADDR=host.docker.internal:6379 \
  sr-cache:latest
```

## Kubernetes

```bash
# 1. Provision the upstream secret out-of-band:
kubectl create secret generic sr-cache-upstream \
  --from-literal=UPSTREAM_URL=https://psrc-xxxxxxx.region.cloud.confluent.cloud \
  --from-literal=UPSTREAM_API_KEY=... \
  --from-literal=UPSTREAM_API_SECRET=...

# 2. Optional dev Valkey (or use a managed Valkey/Redis):
kubectl apply -f k8s/valkey.yaml

# 3. Deploy:
kubectl apply -f k8s/deployment.yaml
kubectl apply -f k8s/service.yaml
kubectl apply -f k8s/hpa.yaml
```

The HPA targets 70% CPU, scales 2 → 20 replicas. The pod is non-root, runs on a
read-only filesystem (no on-disk state), and has Redis-aware readiness so
unhealthy pods get pulled from the LB.

## Design notes

- **Why no boot warmup?** Pre-fetching every schema would burst >150 RPS at the
  upstream and immediately get throttled. Multi-pod warming compounds the
  problem. AOF-backed Redis already gives durability across pod restarts —
  natural traffic respects the budget via singleflight + caching.
- **Why detached fetch context?** The upstream call uses a fresh
  `context.WithTimeout` rather than the inbound request's context. If the
  caller cancels mid-flight, the in-flight upstream call still completes and
  benefits the other goroutines coalesced on the same singleflight key.
- **Why don't we cache errors?** Only 2xx responses are cached. A transient
  upstream 500 or 404 should not pin the wrong answer in Redis.
- **Why a custom breaker?** ~70 lines of code for the exact semantics we need
  (trip on 429s, half-open probe, state callback to a metric) — adding a
  dependency wasn't worth it.

## Operations

For day-2 procedures (rotating creds, recovering from incidents, scaling
beyond defaults, alert recommendations), see **[RUNBOOK.md](RUNBOOK.md)**.

The minimum recommended alerts:

| Alert | Condition |
|---|---|
| `BreakerOpen` | `breaker_state == 1` for 5m |
| `RedisDown` | `redis_up == 0` for 2m |
| `HighUpstreamRPS` | `upstream_rps > 100` for 10m |
| `LowHitRatio` | hit ratio < 0.8 for 30m |
| `HighLatency` | p99 hit latency > 50ms |

## File layout

```
main.go
internal/
  config/        env-driven configuration
  upstream/      basic-auth-injecting HTTP client
  cache/         go-redis wrapper, JSON-serialized entries
  proxy/         handlers + singleflight + invalidation
  breaker/       circuit breaker (closed → open → half-open)
  metrics/       Prometheus collectors + gin middleware
  admin/         embedded dashboard + /admin/stats
k8s/             Deployment, Service, HPA, Valkey StatefulSet
Dockerfile       multi-stage, distroless final image
```
