package metrics

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	CacheHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Cache hits by route group.",
	}, []string{"route"})

	CacheMisses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Cache misses by route group.",
	}, []string{"route"})

	UpstreamRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "upstream_requests_total",
		Help: "Upstream Schema Registry requests by HTTP status.",
	}, []string{"status"})

	RequestLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "request_latency_seconds",
		Help:    "Inbound request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "outcome"})

	UpstreamRPS = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "upstream_rps",
		Help: "Upstream RPS over a 60s rolling window.",
	}, computeUpstreamRPS)

	BreakerState = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "breaker_state",
		Help: "Circuit breaker state: 0=closed, 1=open, 2=half-open.",
	})

	RedisUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "redis_up",
		Help: "1 if Redis ping succeeded most recently, else 0.",
	})

	// RequestsByClientCN is gated behind LABEL_METRICS_BY_CN. CN cardinality
	// can blow up Prometheus if uncapped — only enable when you know your CN
	// space is bounded (<1000 distinct values).
	RequestsByClientCN = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "requests_by_client_cn_total",
		Help: "Inbound requests by client CN and outcome.",
	}, []string{"cn", "outcome"})
)

func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(
		CacheHits, CacheMisses, UpstreamRequests,
		RequestLatency, UpstreamRPS, BreakerState, RedisUp,
		RequestsByClientCN,
	)
}

// --- 60s rolling window for upstream RPS ---

var (
	window     [60]int64 // counts per second-bucket
	windowMu   sync.Mutex
	windowBase int64 // unix-second of bucket[0]
)

func RecordUpstream(status int) {
	UpstreamRequests.WithLabelValues(strconv.Itoa(status)).Inc()
	now := time.Now().Unix()
	windowMu.Lock()
	defer windowMu.Unlock()
	advance(now)
	idx := int(now-windowBase) % len(window)
	window[idx]++
}

func advance(now int64) {
	if windowBase == 0 {
		windowBase = now - int64(len(window)) + 1
		return
	}
	gap := now - (windowBase + int64(len(window)) - 1)
	if gap <= 0 {
		return
	}
	if gap >= int64(len(window)) {
		for i := range window {
			window[i] = 0
		}
		windowBase = now - int64(len(window)) + 1
		return
	}
	for i := int64(0); i < gap; i++ {
		windowBase++
		idx := int(windowBase+int64(len(window))-1) % len(window)
		window[idx] = 0
	}
}

func computeUpstreamRPS() float64 {
	now := time.Now().Unix()
	windowMu.Lock()
	defer windowMu.Unlock()
	advance(now)
	var total int64
	for _, v := range window {
		total += v
	}
	return float64(total) / float64(len(window))
}

// ReadUpstreamRPS returns the current 60s-window upstream RPS. Public so the
// admin dashboard can show it without scraping /metrics.
func ReadUpstreamRPS() float64 { return computeUpstreamRPS() }

// --- liveness counters used by the admin dashboard ---

var (
	totalHits   atomic.Uint64
	totalMisses atomic.Uint64
	totalPass   atomic.Uint64 // passthrough writes
)

func IncHit()    { totalHits.Add(1) }
func IncMiss()   { totalMisses.Add(1) }
func IncPass()   { totalPass.Add(1) }
func ReadHits() uint64   { return totalHits.Load() }
func ReadMisses() uint64 { return totalMisses.Load() }
func ReadPass() uint64   { return totalPass.Load() }

// Middleware records request latency keyed by route + outcome (set via c.Set).
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		outcome := c.GetString("outcome")
		if outcome == "" {
			outcome = "unknown"
		}
		route := c.GetString("route_group")
		if route == "" {
			route = c.FullPath()
			if route == "" {
				route = "other"
			}
		}
		RequestLatency.WithLabelValues(route, outcome).Observe(time.Since(start).Seconds())
	}
}
