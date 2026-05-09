package main

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/akrishnanDG/sr-cache/internal/admin"
	"github.com/akrishnanDG/sr-cache/internal/breaker"
	"github.com/akrishnanDG/sr-cache/internal/cache"
	"github.com/akrishnanDG/sr-cache/internal/config"
	"github.com/akrishnanDG/sr-cache/internal/identity"
	"github.com/akrishnanDG/sr-cache/internal/metrics"
	"github.com/akrishnanDG/sr-cache/internal/proxy"
	"github.com/akrishnanDG/sr-cache/internal/upstream"
)

func main() {
	loadDotenv(".env")

	cfg, err := config.Load()
	if err != nil {
		// Use a minimal logger before config is loaded so we can still surface the error.
		slog.New(slog.NewTextHandler(os.Stdout, nil)).Error("config load failed", "err", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)

	up, err := upstream.New(cfg.UpstreamURL, cfg.UpstreamKey, cfg.UpstreamSecret, cfg.UpstreamTimeout)
	if err != nil {
		slog.Error("upstream init failed", "err", err)
		os.Exit(1)
	}

	rdb, err := cache.NewWithOptions(cache.Options{
		Addr:          cfg.RedisAddr,
		Username:      cfg.RedisUsername,
		Password:      cfg.RedisPassword,
		DB:            cfg.RedisDB,
		PoolSize:      cfg.RedisPoolSize,
		TLS:           cfg.RedisTLS,
		TLSCAFile:     cfg.RedisTLSCAFile,
		TLSSkipVerify: cfg.RedisTLSSkipVerify,
	})
	if err != nil {
		slog.Error("redis init failed", "err", err)
		os.Exit(1)
	}

	// Best-effort initial ping; readyz will reflect ongoing health.
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := rdb.Ping(pingCtx); err != nil {
		slog.Warn("redis ping failed at startup", "err", err, "addr", cfg.RedisAddr)
		metrics.RedisUp.Set(0)
	} else {
		metrics.RedisUp.Set(1)
	}
	cancel()

	br := breaker.New(cfg.BreakerFailThreshold, cfg.BreakerOpenDuration, func(s breaker.State) {
		slog.Warn("circuit breaker state change", "state", s.String())
		var v float64
		switch s {
		case breaker.Closed:
			v = 0
		case breaker.Open:
			v = 1
		case breaker.HalfOpen:
			v = 2
		}
		metrics.BreakerState.Set(v)
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	metrics.MustRegister(reg)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	adminSrv := admin.New(rdb, br)
	r.Use(adminSrv.Middleware())
	r.Use(metrics.Middleware())

	// Operational + scrape endpoints first; never gate them on identity so
	// k8s probes and Prometheus scrapers don't need a CN header.
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})))
	adminSrv.Register(r)

	// Identity middleware applies to the proxied SR API only.
	idCfg := identity.Config{
		Header:           cfg.ClientCNHeader,
		Required:         cfg.ClientCNRequired,
		Allowlist:        cfg.ClientCNAllowlist,
		AuditLog:         cfg.AuditLog,
		LabelMetricsByCN: cfg.LabelMetricsByCN,
	}
	if cfg.ClientCNRequired || len(cfg.ClientCNAllowlist) > 0 || cfg.LabelMetricsByCN {
		slog.Info("client identity middleware enabled",
			"header", idCfg.Header,
			"required", idCfg.Required,
			"allowlist_size", len(idCfg.Allowlist),
			"label_metrics_by_cn", idCfg.LabelMetricsByCN,
			"warning", "header is trusted only when proxy is reachable solely from a verified Ingress",
		)
	}
	r.Use(identity.Middleware(idCfg))

	h := proxy.New(up, rdb, br, cfg.LatestTTL, cfg.ListTTL)
	h.Register(r)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go adminSrv.PollRedis(ctx, 5*time.Second)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr, "upstream", cfg.UpstreamURL, "redis", cfg.RedisAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer sCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown error", "err", err)
	}
	up.CloseIdleConnections()
	if err := rdb.Close(); err != nil {
		slog.Warn("redis close error", "err", err)
	}
	slog.Info("bye")
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// loadDotenv reads simple KEY=VALUE lines from a .env file if it exists,
// without overriding values already in the environment.
func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		if _, ok := os.LookupEnv(k); !ok {
			_ = os.Setenv(k, v)
		}
	}
}
