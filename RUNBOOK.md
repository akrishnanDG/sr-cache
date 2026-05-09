# Operations runbook

What to do when things happen. All commands assume `kubectl` is configured
against the cluster running `sr-cache`.

## Triage checklist

When something breaks, work through in this order — each step rules out
classes of failure:

1. **Pod health** — `kubectl get pods -l app=sr-cache`
   - Pods in `CrashLoopBackOff` → check `kubectl logs --previous`
   - All `Running` but `0/1 Ready` → readiness failing (Redis unreachable)
2. **Redis health** — `kubectl exec deploy/sr-cache -- wget -qO- http://localhost:8080/readyz`
   - 503 with `{"status":"not_ready","redis":"down"}` → Redis problem (next steps)
   - 200 → Redis OK; problem is upstream or downstream
3. **Upstream / breaker** — `kubectl exec deploy/sr-cache -- wget -qO- http://localhost:8080/admin/stats`
   - `breaker: open` → upstream rate-limited or down
   - `breaker: closed`, `upstream_rps` very high → cache miss storm
4. **Hit ratio** — same endpoint
   - `hit_ratio < 0.8` for sustained period → cache cold or TTL too short
5. **Logs** — `kubectl logs deploy/sr-cache --tail=200 | grep -E "ERROR|WARN"`

## Recommended alerts

| Alert | Condition | What it means |
|---|---|---|
| `BreakerOpen` | `breaker_state == 1` for 5m | Upstream is rate-limiting or down |
| `RedisDown` | `redis_up == 0` for 2m | Cache unavailable; every request misses |
| `HighUpstreamRPS` | `upstream_rps > 100` for 10m | Approaching the 150 RPS budget |
| `LowHitRatio` | hit ratio < 0.8 for 30m | Cache not warming or TTLs misconfigured |
| `HighLatency` | `request_latency_seconds{outcome="hit"}` p99 > 50ms | Valkey or network problem |
| `PodCrashLoop` | `kube_pod_container_status_restarts_total` increasing | App-level crash; check logs |
| `AuditLogStopped` | log volume drops to zero | Logging pipeline broken |

## Procedures

### Rotate the upstream Schema Registry API key

When the SR API key needs rotation (quarterly, per Confluent best practice).

1. Generate a new key pair in Confluent Cloud Console (same env, same scope).
2. Update the Secret without disturbing the running pods:
   ```bash
   kubectl create secret generic sr-cache-upstream \
     --from-literal=UPSTREAM_URL="$URL" \
     --from-literal=UPSTREAM_API_KEY="$NEW_KEY" \
     --from-literal=UPSTREAM_API_SECRET="$NEW_SECRET" \
     --dry-run=client -o yaml | kubectl apply -f -
   ```
3. Roll the deployment so pods pick up the new env:
   ```bash
   kubectl rollout restart deploy/sr-cache
   kubectl rollout status deploy/sr-cache --timeout=5m
   ```
4. Verify upstream calls still succeed:
   ```bash
   kubectl exec deploy/sr-cache -- wget -qO- http://localhost:8080/admin/stats
   # Expect: breaker=closed, redis=up, recent upstream_rps non-zero
   ```
5. Once stable, delete the OLD key in Confluent Cloud Console.

> Cache content is **not** invalidated by key rotation. Cached schemas remain
> valid; only new upstream calls use the new key.

### Rotate the Valkey/Redis password (or ACL token)

1. Provision the new password as a *new* Secret key:
   ```bash
   kubectl patch secret sr-cache-redis --type merge -p \
     "{\"stringData\":{\"password-new\":\"$(openssl rand -base64 32)\"}}"
   ```
2. Update Valkey to accept BOTH passwords briefly (Redis ACLs make this easy):
   ```bash
   kubectl exec sts/valkey -- valkey-cli -a "$OLD" \
     ACL SETUSER default on '>OLD_PASSWORD' '>NEW_PASSWORD' '~*' '+@all'
   ```
3. Swap sr-cache to use the new password — rolling restart picks it up.
4. After the rollout, remove the old password:
   ```bash
   kubectl exec sts/valkey -- valkey-cli -a "$NEW" \
     ACL SETUSER default on '<OLD_PASSWORD' '~*' '+@all'
   ```
5. Delete `password-old` from the Secret.

For managed Valkey (ElastiCache / Memorystore), follow the cloud provider's
two-token rotation procedure — same shape.

### Cache poisoning recovery / corrupted cache

If cached data is wrong (rare; SR responses are deterministic but a buggy
cache write or operator error could happen).

1. Identify the bad keys (e.g. all `sr:latest:*`, all `sr:id:*`):
   ```bash
   kubectl exec sts/valkey -- valkey-cli -a "$PASS" --scan --pattern 'sr:latest:*'
   ```
2. Selective purge:
   ```bash
   kubectl exec sts/valkey -- sh -c \
     'valkey-cli -a "$REDIS_PASSWORD" --scan --pattern "sr:latest:*" | xargs -r valkey-cli -a "$REDIS_PASSWORD" DEL'
   ```
3. Nuclear option (full flush):
   ```bash
   kubectl exec sts/valkey -- valkey-cli -a "$PASS" FLUSHDB
   ```
4. Re-fill happens lazily on next traffic — singleflight protects upstream
   from a stampede. Hit ratio dips to ~0% briefly, recovers as keys re-cache.

### Breaker stuck open

Breaker shows `open` for longer than `BREAKER_OPEN_DURATION` (default 10s).

1. Check upstream — is Cloud SR actually rate-limiting?
   ```bash
   curl -u "$UPSTREAM_API_KEY:$UPSTREAM_API_SECRET" \
     https://psrc-xxxxxxx.region.cloud.confluent.cloud/subjects | head -1
   ```
2. If upstream returns 200, breaker should auto-recover on the next probe
   (every 10s). If it doesn't, the breaker is in a half-open + repeated-failure
   loop — restart a single pod to reset its breaker:
   ```bash
   kubectl delete pod <one-pod>
   ```
3. If upstream is still 429, find what's burning your RPS budget:
   - Check `cache_misses_total` per route — which route is missing most?
   - Check `requests_by_client_cn_total` (if `LABEL_METRICS_BY_CN=true`)
     — is one CN responsible?
4. Mitigation while you investigate:
   - Bump `LATEST_TTL` from 30s → 120s (env change + rolling restart) — cuts
     latest-version refresh load 4×.
   - Bump `LIST_TTL` from 10s → 60s.

### Redis OOM / eviction storm

Valkey memory growing → at `maxmemory`, LRU eviction kicks in → hit ratio
drops as evicted keys re-fetch upstream → upstream load increases → maybe
breaker trips.

1. Check Valkey memory: `kubectl exec sts/valkey -- valkey-cli -a "$PASS" INFO memory`
2. Check key count: `kubectl exec sts/valkey -- valkey-cli -a "$PASS" DBSIZE`
3. If keyspace is full of legitimate cache, increase Valkey memory:
   - Update PVC + memory limits in `k8s/valkey.yaml`
   - For managed Valkey, scale up the instance size
4. If you see suspicious keys (e.g. cache cardinality DoS — many
   `sr:subjects:somethingweird` entries), confirm `canonicalQuery` is doing
   its job. Check the `request_latency_seconds_count` route labels — should
   be a small fixed set (`subjects_list`, `schemas_id`, `subject_latest`,
   etc.). If you see hundreds of route values, the query sanitizer is broken
   — file a bug.

### Adding a new client CN to the allowlist

1. Update the `CLIENT_CN_ALLOWLIST` env in `k8s/deployment.yaml`:
   ```yaml
   - name: CLIENT_CN_ALLOWLIST
     value: "app-foo,billing-svc,new-app"
   ```
2. Apply + rolling restart:
   ```bash
   kubectl apply -f k8s/deployment.yaml
   kubectl rollout status deploy/sr-cache
   ```
3. Verify the new CN can reach SR:
   ```bash
   curl -k --cert /path/to/new-app.crt --key /path/to/new-app.key \
     https://sr.example.com/subjects
   ```
4. Audit log will show `cn=new-app outcome=hit` (or miss) — confirm.

### Removing a CN

Same as above but remove the entry. Existing in-flight connections from that
CN aren't terminated automatically — they'll get 403 on next request.

### Upstream Cloud SR is down (real outage)

`breaker_state` flips to open and stays there.

1. **Reads still work** for cached keys — clients with hot subjects
   continue to get cached schemas.
2. **Writes fail** — POST/PUT/DELETE pass through to upstream and 502.
3. **Cache misses fail with 503 + Retry-After** — clients should back off.
4. There is nothing to do at the proxy level. Watch Confluent status page;
   breaker auto-recovers when upstream returns.
5. If outage is prolonged and you need to serve stale data: there is no
   built-in stale-while-revalidate. The cache TTL is the cache TTL.
   `sr:id:*` and numeric versions are forever, so those keep serving.

### Scaling triggers

Beyond what HPA does automatically, watch for:

| Signal | Action |
|---|---|
| HPA stuck at max replicas + CPU > 90% | Bump `maxReplicas` in `k8s/hpa.yaml` and check why per-pod CPU is so high (was payload size?) |
| `upstream_rps > 100` consistently | Bump TTLs (`LATEST_TTL`, `LIST_TTL`) — see "Breaker stuck open" |
| Pod memory near limit | Bump memory limits OR check for unusually large schemas in cache |
| Valkey CPU > 80% | Move to managed Valkey or shard via Valkey Cluster |
| Inbound latency p99 > 50ms on hits | Co-locate Valkey + sr-cache in same AZ; check NetworkPolicy isn't adding overhead |

### Decommissioning a pod gracefully

Beyond `kubectl rollout restart` (which already does this):

1. The deployment has `terminationGracePeriodSeconds: 30` (k8s default).
2. On SIGTERM, sr-cache:
   - Stops accepting new requests
   - Drains in-flight requests for up to 15s
   - Closes idle upstream HTTP connections
   - Closes the Redis pool
3. If you need a longer drain (e.g. for streams holding open connections),
   extend `terminationGracePeriodSeconds` in the pod spec.

## Useful one-liners

```bash
# Live tail of audit log
kubectl logs -f deploy/sr-cache | grep msg=request

# What's cached right now?
kubectl exec sts/valkey -- valkey-cli -a "$PASS" --scan | head -20

# Keys per route (rough cache cardinality)
kubectl exec sts/valkey -- valkey-cli -a "$PASS" --scan | cut -d: -f1-2 | sort | uniq -c

# Cache hit ratio over the last 5 minutes
kubectl exec deploy/sr-cache -- wget -qO- http://localhost:8080/admin/stats | jq .hit_ratio

# Force-reset the breaker on every replica
kubectl rollout restart deploy/sr-cache

# Per-CN traffic breakdown (only if LABEL_METRICS_BY_CN=true)
kubectl exec deploy/sr-cache -- wget -qO- http://localhost:8080/metrics | \
  grep '^requests_by_client_cn_total' | sort -t= -k4 -n
```

## When to escalate

- Breaker open and upstream API direct call also returns 5xx → Confluent
  support ticket (Cloud SR incident).
- All sr-cache replicas crash-looping with the same error → file a bug
  against `sr-cache` repo with logs.
- Valkey memory keeps growing despite `maxmemory-policy: allkeys-lru` →
  cardinality DoS; check audit log for unusual query strings, then revisit
  `canonicalQuery` in `internal/proxy/handler.go`.

## What this runbook does NOT cover

- Confluent Cloud SR configuration (compatibility modes, schema deletion,
  etc.) — that's an SR concern, not a proxy concern.
- Kafka cluster operations.
- Application-level retries / DLQ patterns.

For those, see the corresponding service docs.
