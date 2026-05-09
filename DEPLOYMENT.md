# Deployment guide

`sr-cache` is a single Go binary that talks HTTP on one side and HTTPS+Basic
to Confluent Cloud Schema Registry on the other. It can be deployed in
several topologies depending on what the surrounding network already does for
authentication and TLS termination.

## Decision tree

```
Where is the proxy reachable from?
│
├── Just one app pod (sidecar)
│      → Pattern A — sidecar
│
├── Other pods inside the same k8s cluster (private)
│      ├── plaintext client traffic OK?
│      │      → Pattern B — ClusterIP
│      └── pod-to-pod mTLS via service mesh?
│             → Pattern C — service-mesh-encrypted
│
└── Outside the cluster (apps in another VPC, on-prem, etc.)
       ├── apps already speak mTLS?
       │      → Pattern D — Ingress with mTLS  ← recommended for FF mTLS asks
       └── apps speak plain HTTPS, auth handled by API gateway?
              → Pattern E — Ingress with bearer/JWT/network-restricted

Where does the cache live?
│
├── Dev / staging                  → reference k8s/valkey.yaml manifest
├── Single-region prod, HA needed  → AWS ElastiCache for Valkey / GCP Memorystore for Valkey
├── On-prem / air-gapped           → self-hosted Valkey Sentinel
└── Multi-region active/active     → Redis Enterprise Cloud (or app-level cache key prefixing)
```

## Patterns

### Pattern A — Sidecar

```
┌──────── pod ────────┐
│ ┌────────┐  ┌─────┐ │
│ │  app   │──│sr-  │ │──HTTPS+Basic──▶ Cloud SR
│ │        │  │cache│ │──RESP──▶ Valkey (separate Service)
│ └────────┘  └─────┘ │
└─────────────────────┘
```

- App connects to `http://localhost:8080` — no TLS, no auth.
- Both containers share the same network namespace; nothing else can reach `:8080`.
- `sr-cache` reads upstream creds from a Secret; app never sees them.

**When to pick:** the cache should be tightly bound to one workload
(e.g. a Flink job, a stream processor). Maximum isolation, slight memory
overhead per pod.

**Cache layer:** can be a separate Valkey Service (shared across replicas) or
a small in-pod Valkey for fully isolated state.

**Gotchas:** scale tied to app replica count. HPA on the app, not on
sr-cache, drives cache pod count. Each replica's sr-cache hits the same
Valkey, so cache hit ratio is still good.

---

### Pattern B — ClusterIP (the default)

```
[apps]──HTTP──▶ Service: sr-cache (ClusterIP) ──▶ pods ──▶ Valkey
                                                       │
                                                       └──▶ Cloud SR
```

- Ordinary k8s Service of type `ClusterIP`, port 8080.
- Apps inside the cluster point at `http://sr-cache:8080`.
- No TLS, no auth on the proxy. NetworkPolicy restricts ingress to known
  workloads.

**When to pick:** the cluster boundary is your trust boundary. Most
internal-only deployments land here. This is what `k8s/{deployment,service,
hpa,pdb}.yaml` ship as default.

**Cache layer:** managed Valkey strongly preferred at scale; reference
`k8s/valkey.yaml` works for dev/staging.

**Gotchas:** NetworkPolicy is *required* in multi-tenant clusters — without
it, any pod in the cluster can read/write your schemas. Example policy:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sr-cache-allowed-callers
spec:
  podSelector:
    matchLabels: { app: sr-cache }
  policyTypes: ["Ingress"]
  ingress:
    - from:
        - podSelector:
            matchExpressions:
              - { key: tier, operator: In, values: ["app","stream-processor"] }
      ports:
        - { port: 8080, protocol: TCP }
```

---

### Pattern C — Service mesh (Istio / Linkerd)

```
[app]─sidecar─mTLS─▶ sidecar─[sr-cache] ──▶ Cloud SR
                                  │
                                  └──▶ Valkey (also mesh-enabled)
```

- Pod-to-pod mTLS handled by mesh sidecars.
- sr-cache sees plaintext from a mesh-verified peer.
- Mesh `AuthorizationPolicy` controls which workloads can call sr-cache.
- Optional: have the mesh inject the caller's SPIFFE identity into a header
  → sr-cache reads it as `X-Client-CN` (set `CLIENT_CN_HEADER` accordingly).

**When to pick:** you already run a service mesh. mTLS comes "free" and the
auth surface is enforced by the mesh control plane.

**Cache layer:** managed Valkey, or in-mesh Valkey with mTLS via the mesh.

**Gotchas:** mesh adds 1-3ms latency per hop. Make sure the mesh
`PeerAuthentication` is `STRICT` for the sr-cache namespace.

---

### Pattern D — Ingress with mTLS (recommended for cross-VPC / external clients)

```
[external clients]──mTLS──▶ Ingress (nginx-ingress / Istio Gateway)
                                  │
                                  │ HTTP + X-Client-CN header
                                  ▼
                              sr-cache (ClusterIP) ──▶ Valkey + Cloud SR
```

- Ingress terminates mTLS, validates the client cert against the customer
  CA, extracts CN, forwards as `X-Client-CN`.
- sr-cache trusts the header (proxy is private), audit-logs per CN,
  optionally enforces a CN allowlist.
- Cert lifecycle: cert-manager + your internal CA, or SPIFFE.

**When to pick:** apps live outside the cluster (other VPCs, on-prem,
serverless) **and** must speak mTLS. Useful when the upstream Schema
Registry doesn't natively support mTLS but your clients require it.

**Cache layer:** managed Valkey strongly preferred — these deployments
typically have compliance requirements that disqualify single-instance.

**Gotchas:**
- `NetworkPolicy` must restrict sr-cache so *only* the Ingress controller can
  reach it. Otherwise the `X-Client-CN` header is spoofable.
- Don't enable `LABEL_METRICS_BY_CN=true` if your CN cardinality is unbounded.
- `k8s/ingress.yaml` ships a working nginx-ingress example with the required
  annotations.

**Configuration:**

```yaml
# sr-cache deployment env
env:
  - name: CLIENT_CN_HEADER     # nginx-ingress sometimes uses ssl-client-subject-dn
    value: "X-Client-CN"
  - name: CLIENT_CN_REQUIRED
    value: "true"
  - name: CLIENT_CN_ALLOWLIST
    value: "app-foo,billing-svc,reporting-job"
  - name: AUDIT_LOG
    value: "true"
```

---

### Pattern E — Ingress with bearer/JWT auth or IP allowlist

```
[external clients]──HTTPS──▶ Ingress (with OAuth2 / JWT / IP allowlist)
                                       │
                                       ▼
                                   sr-cache
```

- Ingress validates a JWT (oauth2-proxy, ingress-nginx auth-url) or IP
  allowlist.
- Identity propagated in headers similar to Pattern D.

**When to pick:** clients can't easily speak mTLS but you have an OIDC
provider or a controlled set of caller IPs.

**Gotchas:** same as Pattern D — `NetworkPolicy` is the actual trust
enforcer; the proxy just consumes hint headers.

---

## Cache layer choices

| Option | Pros | Cons | When to pick |
|---|---|---|---|
| Reference `k8s/valkey.yaml` (single instance) | Zero infra dependency | SPOF; no backups; no TLS | Dev / staging / very small prod |
| **AWS ElastiCache for Valkey** | Managed, HA, encryption at rest+in-transit, AWS IAM auth tokens | Costs $; AWS-only | AWS prod |
| **GCP Memorystore for Valkey** | Same, managed | GCP-only | GCP prod |
| **Redis Enterprise Cloud** | Multi-region active/active, commercial support | Costs $$ | Multi-region prod, regulated industries |
| **Self-hosted Valkey Sentinel** | OSS, HA | Operationally heavier | On-prem, air-gapped |
| **Self-hosted Valkey Cluster** | OSS, horizontally sharded | Even more operational complexity | >100k ops/sec sustained |

All of these are drop-in: set `REDIS_ADDR`, optionally `REDIS_USERNAME`,
`REDIS_PASSWORD`, `REDIS_TLS=true`, and sr-cache works unchanged.

## Sizing reference

Per-pod realistic capacity at `requests: 500m / 256Mi`, `limits: 2 / 1Gi`:

| Inbound RPS | Replicas | Upstream RPS (steady) |
|---|---|---|
| 1,000 | 2 | <10 |
| 6,000 | 3 | <30 |
| 20,000 | 6–8 | 50–120 |
| 50,000 | 15–20 | 100–200 (consider tuning `LATEST_TTL` up) |

Above 50k RPS sustained, you'd start thinking about Valkey Cluster, regional
sharding of cache state, and aggressive request budgeting. Most deployments
won't approach this.

## Pre-deployment checklist

- [ ] Upstream SR API key + secret provisioned via external-secrets-operator
      (or equivalent), not stored in git.
- [ ] Cache layer chosen and provisioned (managed > self-hosted Sentinel >
      reference manifest).
- [ ] `REDIS_PASSWORD` (and `REDIS_USERNAME` for ACL) wired via Secret.
- [ ] If Pattern D/E: Ingress controller deployed, server cert via
      cert-manager, client CA bundle in a Secret/ConfigMap.
- [ ] NetworkPolicy restricting sr-cache ingress to authorized callers.
- [ ] HPA limits checked against expected peak traffic + 50% headroom.
- [ ] PodDisruptionBudget present (`k8s/pdb.yaml`).
- [ ] Prometheus scraping `/metrics` — pod annotation already set.
- [ ] Audit log shipped to SIEM (stdout → fluentbit → backend).
- [ ] Alerts wired (see `RUNBOOK.md` for the recommended set).
- [ ] At least one synthetic test (CronJob doing a `/subjects` request every
      minute) so silent failures get detected before customers do.

See `RUNBOOK.md` for what to do once the system is running.
