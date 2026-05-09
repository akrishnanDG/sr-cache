package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	UpstreamURL     string
	UpstreamKey     string
	UpstreamSecret  string
	UpstreamTimeout time.Duration

	RedisAddr         string
	RedisUsername     string
	RedisPassword     string
	RedisDB           int
	RedisPoolSize     int
	RedisTLS          bool
	RedisTLSCAFile    string
	RedisTLSSkipVerify bool

	LatestTTL time.Duration
	ListTTL   time.Duration

	ListenAddr string
	LogLevel   string

	BreakerOpenDuration  time.Duration
	BreakerFailThreshold int

	ClientCNHeader    string
	ClientCNRequired  bool
	ClientCNAllowlist []string
	AuditLog          bool
	LabelMetricsByCN  bool
}

func Load() (*Config, error) {
	c := &Config{
		UpstreamURL:     getenv("UPSTREAM_URL", ""),
		UpstreamKey:     getenv("UPSTREAM_API_KEY", ""),
		UpstreamSecret:  getenv("UPSTREAM_API_SECRET", ""),
		UpstreamTimeout: getDuration("UPSTREAM_TIMEOUT", 10*time.Second),

		RedisAddr:          getenv("REDIS_ADDR", "localhost:6379"),
		RedisUsername:      getenv("REDIS_USERNAME", ""),
		RedisPassword:      getenv("REDIS_PASSWORD", ""),
		RedisDB:            getInt("REDIS_DB", 0),
		RedisPoolSize:      getInt("REDIS_POOL_SIZE", 50),
		RedisTLS:           getBool("REDIS_TLS", false),
		RedisTLSCAFile:     getenv("REDIS_TLS_CA_FILE", ""),
		RedisTLSSkipVerify: getBool("REDIS_TLS_SKIP_VERIFY", false),

		LatestTTL: getDuration("LATEST_TTL", 30*time.Second),
		ListTTL:   getDuration("LIST_TTL", 10*time.Second),

		ListenAddr: getenv("LISTEN_ADDR", ":8080"),
		LogLevel:   getenv("LOG_LEVEL", "info"),

		BreakerOpenDuration:  getDuration("BREAKER_OPEN_DURATION", 10*time.Second),
		BreakerFailThreshold: getInt("BREAKER_FAIL_THRESHOLD", 3),

		ClientCNHeader:    getenv("CLIENT_CN_HEADER", "X-Client-CN"),
		ClientCNRequired:  getBool("CLIENT_CN_REQUIRED", false),
		ClientCNAllowlist: splitCSV(getenv("CLIENT_CN_ALLOWLIST", "")),
		AuditLog:          getBool("AUDIT_LOG", true),
		LabelMetricsByCN:  getBool("LABEL_METRICS_BY_CN", false),
	}

	if c.UpstreamURL == "" {
		return nil, fmt.Errorf("UPSTREAM_URL is required")
	}
	if c.UpstreamKey == "" || c.UpstreamSecret == "" {
		return nil, fmt.Errorf("UPSTREAM_API_KEY and UPSTREAM_API_SECRET are required")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
