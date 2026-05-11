// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	_ "embed"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/akrishnanDG/sr-cache/internal/breaker"
	"github.com/akrishnanDG/sr-cache/internal/cache"
	"github.com/akrishnanDG/sr-cache/internal/metrics"
)

//go:embed dashboard.html
var dashboardHTML []byte

type Server struct {
	Cache   *cache.Cache
	Breaker *breaker.Breaker
	Started time.Time

	// inbound RPS rolling window (60 buckets of 1s)
	inMu      sync.Mutex
	inWindow  [60]int64
	inBase    int64
	redisUp   atomic.Bool
}

func New(c *cache.Cache, b *breaker.Breaker) *Server {
	s := &Server{Cache: c, Breaker: b, Started: time.Now()}
	s.redisUp.Store(true)
	return s
}

// Register wires admin and health routes.
func (s *Server) Register(r *gin.Engine) {
	r.GET("/admin", s.dashboard)
	r.GET("/admin/stats", s.stats)
	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)
}

// Middleware records inbound RPS — wrap on the gin engine.
func (s *Server) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.recordInbound()
		c.Next()
	}
}

func (s *Server) recordInbound() {
	now := time.Now().Unix()
	s.inMu.Lock()
	defer s.inMu.Unlock()
	s.advance(now)
	idx := int(now-s.inBase) % len(s.inWindow)
	s.inWindow[idx]++
}

func (s *Server) advance(now int64) {
	if s.inBase == 0 {
		s.inBase = now - int64(len(s.inWindow)) + 1
		return
	}
	gap := now - (s.inBase + int64(len(s.inWindow)) - 1)
	if gap <= 0 {
		return
	}
	if gap >= int64(len(s.inWindow)) {
		for i := range s.inWindow {
			s.inWindow[i] = 0
		}
		s.inBase = now - int64(len(s.inWindow)) + 1
		return
	}
	for i := int64(0); i < gap; i++ {
		s.inBase++
		idx := int(s.inBase+int64(len(s.inWindow))-1) % len(s.inWindow)
		s.inWindow[idx] = 0
	}
}

func (s *Server) inboundRPS() float64 {
	now := time.Now().Unix()
	s.inMu.Lock()
	defer s.inMu.Unlock()
	s.advance(now)
	var total int64
	for _, v := range s.inWindow {
		total += v
	}
	return float64(total) / float64(len(s.inWindow))
}

// readyDownThreshold is how many consecutive ping failures must occur before
// we mark Redis (and the pod) as not-ready. Single transient timeouts (Redis
// GC pause, network blip) won't flip readiness.
const readyDownThreshold = 3

// PollRedis runs in the background and updates redis_up + readyz state. It
// requires `readyDownThreshold` consecutive failures before flipping
// redis_up=false to avoid evicting healthy pods on transient blips.
func (s *Server) PollRedis(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := s.Cache.Ping(pingCtx)
			cancel()
			if err == nil {
				consecutiveFailures = 0
				if !s.redisUp.Load() {
					s.redisUp.Store(true)
				}
				metrics.RedisUp.Set(1)
				continue
			}
			consecutiveFailures++
			if consecutiveFailures >= readyDownThreshold {
				s.redisUp.Store(false)
				metrics.RedisUp.Set(0)
			}
		}
	}
}

func (s *Server) dashboard(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", dashboardHTML)
}

type statsResponse struct {
	RPS         float64 `json:"rps"`
	UpstreamRPS float64 `json:"upstream_rps"`
	HitRatio    float64 `json:"hit_ratio"`
	Hits        uint64  `json:"hits"`
	Misses      uint64  `json:"misses"`
	Pass        uint64  `json:"pass"`
	Breaker     string  `json:"breaker"`
	Redis       string  `json:"redis"`
	UptimeS     int64   `json:"uptime_s"`
}

func (s *Server) stats(c *gin.Context) {
	hits := metrics.ReadHits()
	misses := metrics.ReadMisses()
	var ratio float64
	if total := hits + misses; total > 0 {
		ratio = float64(hits) / float64(total)
	}
	redis := "down"
	if s.redisUp.Load() {
		redis = "up"
	}
	upstreamRPS := metrics.ReadUpstreamRPS()
	c.JSON(http.StatusOK, statsResponse{
		RPS:         s.inboundRPS(),
		UpstreamRPS: upstreamRPS,
		HitRatio:    ratio,
		Hits:        hits,
		Misses:      misses,
		Pass:        metrics.ReadPass(),
		Breaker:     s.Breaker.State().String(),
		Redis:       redis,
		UptimeS:     int64(time.Since(s.Started).Seconds()),
	})
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) readyz(c *gin.Context) {
	if !s.redisUp.Load() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "redis": "down"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}
