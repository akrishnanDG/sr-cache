package identity_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/akrishnanDG/sr-cache/internal/identity"
)

func init() { gin.SetMode(gin.TestMode) }

func newRig(cfg identity.Config) *gin.Engine {
	r := gin.New()
	r.Use(identity.Middleware(cfg))
	r.GET("/test", func(c *gin.Context) {
		cn := identity.GetCN(c)
		c.JSON(http.StatusOK, gin.H{"cn": cn})
	})
	return r
}

func do(r *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestExtractsCN_DefaultHeader(t *testing.T) {
	r := newRig(identity.Config{AuditLog: false})
	w := do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": "app-foo"})
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if got, want := w.Body.String(), `{"cn":"app-foo"}`; got != want {
		t.Fatalf("body=%q want %q", got, want)
	}
}

func TestExtractsCN_CustomHeader(t *testing.T) {
	r := newRig(identity.Config{Header: "X-SSL-Client-S-DN-CN", AuditLog: false})
	w := do(r, http.MethodGet, "/test", map[string]string{"X-SSL-Client-S-DN-CN": "billing"})
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if got, want := w.Body.String(), `{"cn":"billing"}`; got != want {
		t.Fatalf("body=%q want %q", got, want)
	}
}

func TestRequired_MissingHeader_Returns401(t *testing.T) {
	r := newRig(identity.Config{Required: true, AuditLog: false})
	w := do(r, http.MethodGet, "/test", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
}

func TestRequired_HeaderPresent_PassesThrough(t *testing.T) {
	r := newRig(identity.Config{Required: true, AuditLog: false})
	w := do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": "ok"})
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestAllowlist_DenyUnknownCN(t *testing.T) {
	r := newRig(identity.Config{Allowlist: []string{"app-foo", "billing"}, AuditLog: false})
	w := do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": "intruder"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
}

func TestAllowlist_AllowKnownCN(t *testing.T) {
	r := newRig(identity.Config{Allowlist: []string{"app-foo", "billing"}, AuditLog: false})
	w := do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": "billing"})
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestAllowlist_EmptyHeader_BypassesCheck(t *testing.T) {
	// allowlist set but Required=false: empty header → anonymous, allowed
	r := newRig(identity.Config{Allowlist: []string{"app-foo"}, AuditLog: false})
	w := do(r, http.MethodGet, "/test", nil)
	if w.Code != 200 {
		t.Fatalf("status %d, want 200 anonymous", w.Code)
	}
}

func TestAllowlist_RequiredAndEnforced(t *testing.T) {
	// Required + allowlist: missing → 401, present-but-unlisted → 403
	r := newRig(identity.Config{Required: true, Allowlist: []string{"app-foo"}, AuditLog: false})

	w := do(r, http.MethodGet, "/test", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing: status %d, want 401", w.Code)
	}
	w = do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": "intruder"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("intruder: status %d, want 403", w.Code)
	}
	w = do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": "app-foo"})
	if w.Code != 200 {
		t.Fatalf("app-foo: status %d, want 200", w.Code)
	}
}

func TestCNLengthCapped(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'A'
	}
	r := newRig(identity.Config{AuditLog: false})
	w := do(r, http.MethodGet, "/test", map[string]string{"X-Client-CN": string(long)})
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	// returned CN should be truncated to 256
	if len(w.Body.String()) > 256+len(`{"cn":""}`) {
		t.Fatalf("CN not capped, body length %d", len(w.Body.String()))
	}
}

func TestAnonymous_NoHeader_PassesThrough(t *testing.T) {
	r := newRig(identity.Config{AuditLog: false})
	w := do(r, http.MethodGet, "/test", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if got, want := w.Body.String(), `{"cn":""}`; got != want {
		t.Fatalf("body=%q want %q", got, want)
	}
}
