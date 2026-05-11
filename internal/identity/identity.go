// SPDX-License-Identifier: Apache-2.0

// Package identity provides client-identity middleware for the SR cache proxy.
//
// The proxy itself does not terminate TLS or verify client certificates — that
// is delegated to a frontend (nginx-ingress, Envoy, Istio gateway, ...). The
// frontend forwards the verified client identity (typically the certificate
// CN) as an HTTP header. This middleware:
//
//   - reads the configured header into the request context,
//   - optionally rejects requests missing the header (CLIENT_CN_REQUIRED),
//   - optionally enforces an allowlist of permitted CNs,
//   - emits a structured audit log line per request,
//   - optionally labels Prometheus metrics with the CN.
//
// Trust model: the header is HEARSAY — anyone with network reach to the proxy
// could set it. Deploy the proxy on a private interface so only the verified
// frontend can reach it, and rely on the frontend's mTLS verification as the
// actual authentication step.
package identity

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/akrishnanDG/sr-cache/internal/metrics"
)

// ContextKey is the gin.Context key under which the verified CN is stored.
const ContextKey = "client_cn"

const anonymousCN = "anonymous"

type Config struct {
	Header           string   // header to read; default X-Client-CN
	Required         bool     // 401 if header is missing
	Allowlist        []string // empty = allow any CN
	AuditLog         bool     // emit per-request audit line
	LabelMetricsByCN bool     // add cn label to Prometheus
}

// Middleware returns a gin handler implementing the identity policy.
func Middleware(cfg Config) gin.HandlerFunc {
	header := cfg.Header
	if header == "" {
		header = "X-Client-CN"
	}
	allow := make(map[string]struct{}, len(cfg.Allowlist))
	for _, s := range cfg.Allowlist {
		s = strings.TrimSpace(s)
		if s != "" {
			allow[s] = struct{}{}
		}
	}
	enforce := len(allow) > 0

	return func(c *gin.Context) {
		start := time.Now()
		cn := strings.TrimSpace(c.GetHeader(header))

		// Hard cap on CN length; defense against pathological headers.
		if len(cn) > 256 {
			cn = cn[:256]
		}

		switch {
		case cn == "" && cfg.Required:
			c.Set("outcome", "auth_missing")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error_code": http.StatusUnauthorized,
				"message":    "missing client identity header " + header,
			})
		case cn == "":
			c.Set(ContextKey, "")
		case enforce:
			c.Set(ContextKey, cn)
			if _, ok := allow[cn]; !ok {
				c.Set("outcome", "auth_denied")
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error_code": http.StatusForbidden,
					"message":    "client CN '" + cn + "' not authorized",
				})
			}
		default:
			c.Set(ContextKey, cn)
		}

		if c.IsAborted() {
			audit(cfg, c, cn, start)
			recordCNMetric(cfg, c, cn)
			return
		}

		c.Next()

		audit(cfg, c, cn, start)
		recordCNMetric(cfg, c, cn)
	}
}

func audit(cfg Config, c *gin.Context, cn string, start time.Time) {
	if !cfg.AuditLog {
		return
	}
	logCN := cn
	if logCN == "" {
		logCN = anonymousCN
	}
	slog.Info("request",
		"cn", logCN,
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"status", c.Writer.Status(),
		"outcome", c.GetString("outcome"),
		"latency_ms", time.Since(start).Milliseconds(),
	)
}

func recordCNMetric(cfg Config, c *gin.Context, cn string) {
	if !cfg.LabelMetricsByCN {
		return
	}
	if cn == "" {
		cn = anonymousCN
	}
	outcome := c.GetString("outcome")
	if outcome == "" {
		outcome = "unknown"
	}
	metrics.RequestsByClientCN.WithLabelValues(cn, outcome).Inc()
}

// GetCN returns the verified CN associated with the request, or "" for anonymous.
func GetCN(c *gin.Context) string {
	return c.GetString(ContextKey)
}
