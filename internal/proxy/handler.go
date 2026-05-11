// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"

	"github.com/akrishnanDG/sr-cache/internal/breaker"
	"github.com/akrishnanDG/sr-cache/internal/cache"
	"github.com/akrishnanDG/sr-cache/internal/metrics"
	"github.com/akrishnanDG/sr-cache/internal/upstream"
)

// allowedQueryParams enumerates the SR query parameters we want to honor in
// cache keys + forward upstream. Anything else is dropped so that arbitrary
// `?cachebust=N` traffic can't blow up the keyspace.
var allowedQueryParams = map[string]struct{}{
	// Read-side
	"deleted":             {},
	"deletedOnly":         {},
	"latestOnly":          {},
	"normalize":           {},
	"format":              {},
	"defaultToGlobal":     {},
	"subjectPrefix":       {},
	"subject":             {},
	"limit":               {},
	"offset":              {},
	"showDeleted":         {},
	"includeDeleted":      {},
	"fetchMaxId":          {},
	"verbose":             {},
	"lookupDeletedSchema": {},
	// Write-side (must propagate or write semantics break silently)
	"permanent": {}, // DELETE …?permanent=true → hard-delete; without this it silently soft-deletes
	"force":     {}, // force-register schemas / config changes
	"id":        {}, // explicit schema id on POST /subjects/{sub}/versions
	"version":   {}, // explicit version on POST /subjects/{sub}/versions
}

type Handler struct {
	Up        *upstream.Client
	Cache     *cache.Cache
	Breaker   *breaker.Breaker
	LatestTTL time.Duration
	ListTTL   time.Duration

	sf singleflight.Group
}

func New(up *upstream.Client, c *cache.Cache, b *breaker.Breaker, latest, list time.Duration) *Handler {
	return &Handler{Up: up, Cache: c, Breaker: b, LatestTTL: latest, ListTTL: list}
}

// Register wires routes onto the given router group.
func (h *Handler) Register(r *gin.Engine) {
	// Cached GETs — order matters; more specific paths first.
	r.GET("/schemas/ids/:id", h.getSchemaByID)
	r.GET("/schemas/ids/:id/schema", h.getSchemaByIDRaw)
	r.GET("/subjects", h.listSubjects)
	r.GET("/subjects/:subject/versions", h.listVersions)
	r.GET("/subjects/:subject/versions/:version", h.getSubjectVersion)
	r.GET("/config", h.getConfig)
	r.GET("/config/:subject", h.getSubjectConfig)
	r.GET("/mode", h.getMode)
	r.GET("/mode/:subject", h.getSubjectMode)

	// Catch-all for everything else (writes, unknown reads): forward without caching.
	r.NoRoute(h.passthrough)
}

// --- cached GET handlers ---

func (h *Handler) getSchemaByID(c *gin.Context) {
	id := c.Param("id")
	h.cachedGET(c, "schemas_id", "sr:id:"+id, 0)
}

func (h *Handler) getSchemaByIDRaw(c *gin.Context) {
	id := c.Param("id")
	h.cachedGET(c, "schemas_id_raw", "sr:id:"+id+":raw", 0)
}

func (h *Handler) listSubjects(c *gin.Context) {
	h.cachedGET(c, "subjects_list", "sr:subjects:"+canonicalQuery(c.Request.URL.Query()), h.ListTTL)
}

func (h *Handler) listVersions(c *gin.Context) {
	sub := c.Param("subject")
	h.cachedGET(c, "subject_versions", "sr:subject:"+sub+":versions:"+canonicalQuery(c.Request.URL.Query()), h.ListTTL)
}

func (h *Handler) getSubjectVersion(c *gin.Context) {
	sub := c.Param("subject")
	v := c.Param("version")
	q := canonicalQuery(c.Request.URL.Query())
	if v == "latest" || v == "-1" {
		h.cachedGET(c, "subject_latest", "sr:latest:"+sub+":"+q, h.LatestTTL)
		return
	}
	if _, err := strconv.Atoi(v); err == nil {
		// numeric version is immutable
		h.cachedGET(c, "subject_version", "sr:subject:"+sub+":v:"+v+":"+q, 0)
		return
	}
	// unknown — pass through
	h.passthrough(c)
}

func (h *Handler) getConfig(c *gin.Context) {
	h.cachedGET(c, "config", "sr:config:global", h.ListTTL)
}

func (h *Handler) getSubjectConfig(c *gin.Context) {
	sub := c.Param("subject")
	h.cachedGET(c, "subject_config", "sr:config:"+sub, h.ListTTL)
}

func (h *Handler) getMode(c *gin.Context) {
	h.cachedGET(c, "mode", "sr:mode:global", h.ListTTL)
}

func (h *Handler) getSubjectMode(c *gin.Context) {
	sub := c.Param("subject")
	h.cachedGET(c, "subject_mode", "sr:mode:"+sub, h.ListTTL)
}

// cachedGET implements the read-through pattern with singleflight.
func (h *Handler) cachedGET(c *gin.Context, route, key string, ttl time.Duration) {
	c.Set("route_group", route)
	ctx := c.Request.Context()

	// 1) Try cache.
	if entry, ok, err := h.Cache.Get(ctx, key); err == nil && ok {
		metrics.CacheHits.WithLabelValues(route).Inc()
		metrics.IncHit()
		c.Set("outcome", "hit")
		writeEntry(c, entry)
		return
	}

	// 2) Miss: respect breaker.
	metrics.CacheMisses.WithLabelValues(route).Inc()
	metrics.IncMiss()
	c.Set("outcome", "miss")

	if !h.Breaker.Allow() {
		c.Set("outcome", "breaker_open")
		c.Header("Retry-After", "5")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error_code": http.StatusServiceUnavailable,
			"message":    "upstream circuit breaker open; cached value not available",
		})
		return
	}

	// 3) Singleflight fetch.
	v, err, _ := h.sf.Do(key, func() (any, error) {
		return h.fetchAndCache(ctx, c, key, ttl)
	})
	if err != nil {
		c.Set("outcome", "error")
		status := http.StatusBadGateway
		msg := "upstream unavailable"
		if err == errRateLimited {
			status = http.StatusServiceUnavailable
			msg = "upstream rate limited"
			c.Header("Retry-After", "5")
		}
		c.JSON(status, gin.H{"error_code": status, "message": msg})
		return
	}
	entry := v.(*cache.Entry)
	writeEntry(c, entry)
}

// upstreamErr is a sentinel returned to keep upstream details out of the
// client-facing JSON. Real details are logged via slog at error level.
type upstreamErr struct{ kind string }

func (e *upstreamErr) Error() string { return e.kind }

var (
	errUpstream      = &upstreamErr{kind: "upstream_unavailable"}
	errRateLimited   = &upstreamErr{kind: "upstream_rate_limited"}
)

func (h *Handler) fetchAndCache(ctx context.Context, c *gin.Context, key string, ttl time.Duration) (*cache.Entry, error) {
	// Detached context: the upstream call survives even if the originating
	// request is cancelled — the result still benefits other coalesced waiters.
	fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := h.Up.Do(fetchCtx, http.MethodGet, c.Request.URL.Path, sanitizeQuery(c.Request.URL.Query()), c.Request.Header, nil)
	if err != nil {
		// Transport-level errors (TCP reset, DNS failure, timeout) MUST trip
		// the breaker too — otherwise a HalfOpen probe that fails with a
		// network error leaves the breaker stuck HalfOpen, returning false
		// for every subsequent Allow() until process restart.
		slog.Error("upstream call failed", "err", err, "path", c.Request.URL.Path)
		h.Breaker.OnFailure()
		return nil, errUpstream
	}
	metrics.RecordUpstream(resp.Status)

	if resp.IsRateLimited() {
		h.Breaker.OnFailure()
		return nil, errRateLimited
	}
	h.Breaker.OnSuccess()

	entry := &cache.Entry{
		Status:      resp.Status,
		ContentType: resp.ContentType,
		Body:        resp.Body,
	}
	// Only cache successful responses; errors should not be sticky.
	// Use the detached fetchCtx so the cache write survives originator
	// cancellation — the work shouldn't be wasted just because the caller
	// disconnected.
	if resp.Status >= 200 && resp.Status < 300 {
		_ = h.Cache.Set(fetchCtx, key, entry, ttl)
	}
	return entry, nil
}

// passthrough forwards any non-cached request directly upstream.
func (h *Handler) passthrough(c *gin.Context) {
	c.Set("route_group", "passthrough")
	c.Set("outcome", "pass")
	metrics.IncPass()

	if !h.Breaker.Allow() {
		c.Header("Retry-After", "5")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error_code": http.StatusServiceUnavailable,
			"message":    "upstream circuit breaker open",
		})
		return
	}

	var body io.Reader
	if c.Request.Body != nil {
		body = c.Request.Body
	}
	resp, err := h.Up.Do(c.Request.Context(), c.Request.Method, c.Request.URL.Path, sanitizeQuery(c.Request.URL.Query()), c.Request.Header, body)
	if err != nil {
		slog.Error("upstream passthrough failed", "err", err, "method", c.Request.Method, "path", c.Request.URL.Path)
		h.Breaker.OnFailure()
		c.JSON(http.StatusBadGateway, gin.H{"error_code": http.StatusBadGateway, "message": "upstream unavailable"})
		return
	}
	metrics.RecordUpstream(resp.Status)
	if resp.IsRateLimited() {
		h.Breaker.OnFailure()
	} else {
		h.Breaker.OnSuccess()
	}

	// Best-effort invalidation on successful writes.
	if c.Request.Method != http.MethodGet && resp.Status >= 200 && resp.Status < 300 {
		hardDelete := c.Request.Method == http.MethodDelete &&
			c.Request.URL.Query().Get("permanent") == "true"
		h.invalidate(c.Request.Context(), c.Request.Method, c.Request.URL.Path, hardDelete)
	}

	c.Data(resp.Status, fallbackContentType(resp.ContentType), resp.Body)
}

// invalidate drops cache keys likely to be stale after a write.
// It's best-effort — any error is logged elsewhere via metrics.
//
// hardDelete signals that the write was a hard-delete (`?permanent=true`)
// rather than a soft-delete; in that case we must also drop the
// schema-by-ID cache (`sr:id:*`) since the schema body itself is gone.
func (h *Handler) invalidate(ctx context.Context, method, path string, hardDelete bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "subjects":
		// /subjects/:sub/...
		if len(parts) >= 2 {
			sub := parts[1]
			_ = h.Cache.DelByPattern(ctx, "sr:latest:"+sub+":*")
			_ = h.Cache.DelByPattern(ctx, "sr:subject:"+sub+":versions:*")
			_ = h.Cache.DelByPattern(ctx, "sr:subjects:*")
			if hardDelete {
				// Schema bodies are gone upstream; flush schema-by-ID caches
				// and per-version caches for this subject. We use a broad
				// glob for sr:id because IDs aren't deterministic from path.
				_ = h.Cache.DelByPattern(ctx, "sr:subject:"+sub+":v:*")
				_ = h.Cache.DelByPattern(ctx, "sr:id:*")
			}
		} else {
			_ = h.Cache.DelByPattern(ctx, "sr:subjects:*")
		}
	case "schemas":
		// /schemas/ids/:id — hard delete on a schema id explicitly
		if hardDelete && len(parts) >= 3 && parts[1] == "ids" {
			id := parts[2]
			_ = h.Cache.Del(ctx, "sr:id:"+id, "sr:id:"+id+":raw")
		}
	case "config":
		// Compatibility changes can move "latest" semantics — drop subject's
		// latest cache too. Global config affects every subject's latest.
		if len(parts) == 1 {
			_ = h.Cache.Del(ctx, "sr:config:global")
			_ = h.Cache.DelByPattern(ctx, "sr:latest:*")
		} else {
			sub := parts[1]
			_ = h.Cache.Del(ctx, "sr:config:"+sub)
			_ = h.Cache.DelByPattern(ctx, "sr:latest:"+sub+":*")
		}
	case "mode":
		// Similar reasoning: switching READONLY/READWRITE/IMPORT can change
		// which version is current for clients.
		if len(parts) == 1 {
			_ = h.Cache.Del(ctx, "sr:mode:global")
			_ = h.Cache.DelByPattern(ctx, "sr:latest:*")
		} else {
			sub := parts[1]
			_ = h.Cache.Del(ctx, "sr:mode:"+sub)
			_ = h.Cache.DelByPattern(ctx, "sr:latest:"+sub+":*")
		}
	}
}

// canonicalQuery filters URL query params down to the SR-recognized set and
// returns a deterministic encoding. This protects the cache key from
// arbitrary client-controlled cardinality (e.g. `?cachebust=N`).
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	out := url.Values{}
	for k, vs := range q {
		if _, ok := allowedQueryParams[k]; !ok {
			continue
		}
		// Sort values for deterministic key shape.
		sorted := append([]string(nil), vs...)
		sort.Strings(sorted)
		out[k] = sorted
	}
	return out.Encode()
}

// sanitizeQuery returns a url.Values with only allowlisted params, suitable
// for forwarding upstream. We never propagate arbitrary client query params
// because they (a) bloat upstream traffic and (b) could be probes.
func sanitizeQuery(q url.Values) url.Values {
	if len(q) == 0 {
		return nil
	}
	out := url.Values{}
	for k, vs := range q {
		if _, ok := allowedQueryParams[k]; ok {
			out[k] = vs
		}
	}
	return out
}

func writeEntry(c *gin.Context, e *cache.Entry) {
	c.Data(e.Status, fallbackContentType(e.ContentType), e.Body)
}

func fallbackContentType(ct string) string {
	if ct == "" {
		return "application/vnd.schemaregistry.v1+json"
	}
	return ct
}
