// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"

	"github.com/akrishnanDG/sr-cache/internal/breaker"
	"github.com/akrishnanDG/sr-cache/internal/cache"
	"github.com/akrishnanDG/sr-cache/internal/upstream"
)

func newTestRig(t *testing.T, upstreamHandler http.Handler) (*gin.Engine, *miniredis.Miniredis, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(upstreamHandler)
	up, err := upstream.New(ts.URL, "k", "s", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.NewWithOptions(cache.Options{Addr: mr.Addr(), PoolSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	br := breaker.New(3, 100*time.Millisecond, nil)
	h := New(up, c, br, 30*time.Second, 10*time.Second)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r)
	return r, mr, func() {
		ts.Close()
		mr.Close()
		_ = c.Close()
	}
}

func TestSingleflightCoalescesConcurrentMisses(t *testing.T) {
	var upstreamHits atomic.Int64
	gate := make(chan struct{})
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate // hold to maximize concurrency
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"schema":"{\"type\":\"string\"}"}`))
	})
	r, _, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/schemas/ids/1", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		}()
	}
	// give goroutines time to land in the singleflight group, then release.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	got := upstreamHits.Load()
	if got == 0 || got > 5 {
		// Allow a small upper bound: singleflight only coalesces calls that
		// land while the leader is in-flight; a few might race past the gate.
		t.Fatalf("expected upstream hits ≤ 5 (singleflight), got %d", got)
	}
}

func TestImmutableVersionCachedForever(t *testing.T) {
	var hits atomic.Int64
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":5}`))
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/subjects/foo/versions/5", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("iter %d status %d", i, w.Code)
		}
	}
	if h := hits.Load(); h != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", h)
	}

	// Confirm the key has no TTL (Redis returns -1 for "no expire").
	keys := mr.Keys()
	var found string
	for _, k := range keys {
		if len(k) > len("sr:subject:") && k[:len("sr:subject:")] == "sr:subject:" {
			found = k
			break
		}
	}
	if found == "" {
		t.Fatal("no sr:subject key found")
	}
	ttl := mr.TTL(found)
	if ttl != 0 {
		t.Fatalf("expected no TTL on immutable key %s, got %v", found, ttl)
	}
}

func TestLatestVersionUsesShortTTL(t *testing.T) {
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":5}`))
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/subjects/foo/versions/latest", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}

	var found string
	for _, k := range mr.Keys() {
		if len(k) >= len("sr:latest:") && k[:len("sr:latest:")] == "sr:latest:" {
			found = k
			break
		}
	}
	if found == "" {
		t.Fatal("no sr:latest key")
	}
	if ttl := mr.TTL(found); ttl <= 0 || ttl > 30*time.Second {
		t.Fatalf("expected TTL within (0, 30s], got %v", ttl)
	}
}

func TestWritePassesThrough(t *testing.T) {
	var sawMethod string
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod = r.Method
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":42}`))
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/subjects/foo/versions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if sawMethod != http.MethodPost {
		t.Fatalf("upstream saw %q", sawMethod)
	}
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	// Should not have cached anything.
	for _, k := range mr.Keys() {
		t.Fatalf("unexpected cached key after POST: %s", k)
	}
}

func TestWriteInvalidatesCacheKeyShape(t *testing.T) {
	// Prove that the DelByPattern glob in invalidate() actually matches the
	// key shape that cachedGET writes. An off-by-one in either is invisible
	// without this round-trip.
	var listCalls atomic.Int64
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/subjects":
			listCalls.Add(1)
			_, _ = w.Write([]byte(`["foo"]`))
		case r.Method == http.MethodPost && r.URL.Path == "/subjects/foo/versions":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			w.WriteHeader(404)
		}
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	// 1) Prime cache: GET /subjects → upstream call #1.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subjects", nil))
	if listCalls.Load() != 1 {
		t.Fatalf("expected 1 upstream list call, got %d", listCalls.Load())
	}
	if !mr.Exists("sr:subjects:") {
		t.Fatalf("expected cache key sr:subjects: to exist; have %v", mr.Keys())
	}

	// 2) Write: POST /subjects/foo/versions → invalidate must drop sr:subjects:*
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/subjects/foo/versions", nil))
	if w.Code != 200 {
		t.Fatalf("write status %d", w.Code)
	}
	if mr.Exists("sr:subjects:") {
		t.Fatalf("invalidate failed: sr:subjects: should be gone after POST")
	}

	// 3) Re-fetch: GET /subjects → must hit upstream again (call #2).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subjects", nil))
	if listCalls.Load() != 2 {
		t.Fatalf("expected re-fetch upstream call after invalidation, got %d total", listCalls.Load())
	}
}

func TestUpstream5xxNotCached_NotBreakerTrip(t *testing.T) {
	var hits atomic.Int64
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error_code":50001,"message":"internal"}`))
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/schemas/ids/"+strconv.Itoa(7000+i), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
	if hits.Load() != 5 {
		t.Fatalf("expected each 500 to hit upstream (no caching of errors), got %d/5", hits.Load())
	}
	// Nothing cached.
	if len(mr.Keys()) > 0 {
		t.Fatalf("expected no cache keys for 5xx upstream, got %v", mr.Keys())
	}
	// 5xx should NOT trip the breaker (only 429 does).
	// We can't read breaker state directly via the handler's wiring here, but
	// the next GET should still go upstream rather than hitting the breaker.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/schemas/ids/9999", nil))
	if hits.Load() != 6 {
		t.Fatalf("breaker appears tripped: subsequent GET did not reach upstream (hits=%d)", hits.Load())
	}
}

func TestLatestTTLActuallyExpires(t *testing.T) {
	// miniredis FastForward fires the TTL even though wall-clock didn't move,
	// proving the entry would re-fetch upstream after TTL.
	var hits atomic.Int64
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":7}`))
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subjects/foo/versions/latest", nil))
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit pre-expiry, got %d", hits.Load())
	}

	// Fast-forward past the LATEST_TTL (rig sets it to 30s).
	mr.FastForward(31 * time.Second)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/subjects/foo/versions/latest", nil))
	if hits.Load() != 2 {
		t.Fatalf("expected re-fetch after TTL expiry, got %d total", hits.Load())
	}
}

func TestSingleflightLeaderCancellation(t *testing.T) {
	// Verify that the detached fetch context survives originator cancellation:
	// the leader call still completes, fills the cache, and unblocks coalesced
	// followers. We exercise this by cancelling the leader's request context
	// before the upstream responds and asserting the entry still lands in
	// Redis (i.e. the work was not abandoned).
	gate := make(chan struct{})
	var hits atomic.Int64
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":42}`))
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/schemas/ids/1234", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		close(leaderDone)
	}()

	time.Sleep(30 * time.Millisecond) // ensure leader entered singleflight
	cancel()                          // simulate client disconnect
	close(gate)                       // upstream responds
	<-leaderDone

	// Even though the leader's request context was cancelled, the detached
	// fetch context should have completed and persisted the entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, k := range mr.Keys() {
			if k == "sr:id:1234" {
				if hits.Load() != 1 {
					t.Fatalf("upstream hits = %d, want 1", hits.Load())
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("entry never landed in cache after leader cancelled; keys=%v hits=%d", mr.Keys(), hits.Load())
}

// Regression for bug "HalfOpen deadlock on transport errors":
// when the upstream call returns a network error (not a 429), the breaker
// must still record a failure. Otherwise a HalfOpen probe that fails with
// a transport error leaves the breaker stuck HalfOpen forever.
func TestTransportErrorTripsBreaker(t *testing.T) {
	// Stub upstream that closes the connection mid-request → io error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer ts.Close()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	up, err := upstream.New(ts.URL, "k", "s", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.NewWithOptions(cache.Options{Addr: mr.Addr(), PoolSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := breaker.New(2, time.Second, nil)
	h := New(up, c, br, 30*time.Second, 10*time.Second)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r)

	// Two failed transport calls → breaker should transition to Open.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/schemas/ids/1", nil))
	}
	if got := br.State(); got != breaker.Open {
		t.Fatalf("breaker state after 2 transport failures = %s, want Open", got)
	}
}

// Regression for bug "permanent query param stripped":
// DELETE /subjects/foo?permanent=true must propagate the param upstream;
// otherwise a hard-delete silently becomes a soft-delete.
func TestPermanentQueryParamPropagated(t *testing.T) {
	var sawPermanent string
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPermanent = r.URL.Query().Get("permanent")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[1]`))
	})
	r, _, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/subjects/foo?permanent=true", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if sawPermanent != "true" {
		t.Fatalf("upstream saw permanent=%q, want %q", sawPermanent, "true")
	}
}

// Regression for bug "sr:id cache not invalidated on hard-delete":
// a DELETE with permanent=true must drop schema-by-ID caches.
func TestHardDeleteInvalidatesSchemaIDCache(t *testing.T) {
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/schemas/ids/42":
			_, _ = w.Write([]byte(`{"schema":"\"string\""}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`[1]`))
		default:
			w.WriteHeader(404)
		}
	})
	r, mr, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	// 1) Prime the schema-by-ID cache.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/schemas/ids/42", nil))
	if !mr.Exists("sr:id:42") {
		t.Fatalf("expected sr:id:42 to be cached; keys = %v", mr.Keys())
	}

	// 2) Soft-delete (no permanent): sr:id:* must REMAIN cached.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/subjects/foo", nil))
	if !mr.Exists("sr:id:42") {
		t.Fatalf("soft-delete should leave sr:id:42 in cache (schema still resolvable upstream)")
	}

	// 3) Hard-delete (permanent=true): sr:id:* must be flushed.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/subjects/foo?permanent=true", nil))
	if mr.Exists("sr:id:42") {
		t.Fatalf("hard-delete failed to flush sr:id:42 from cache")
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	var hits atomic.Int64
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error_code":40403,"message":"Schema not found"}`))
	})
	r, _, cleanup := newTestRig(t, upstreamH)
	defer cleanup()

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/schemas/ids/"+strconv.Itoa(99999+i), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
	// 3 distinct ids → 3 upstream hits, none cached.
	if h := hits.Load(); h != 3 {
		t.Fatalf("expected 3 upstream hits for 3 distinct missing ids, got %d", h)
	}
}
