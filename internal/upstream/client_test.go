package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClient_InjectsBasicAuthAndPath(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "key", "secret", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(context.Background(), http.MethodGet, "/schemas/ids/1", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d", resp.Status)
	}
	// "Basic " + base64("key:secret") = "Basic a2V5OnNlY3JldA=="
	if gotAuth != "Basic a2V5OnNlY3JldA==" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotPath != "/schemas/ids/1" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q", gotMethod)
	}
}

func TestClient_DetectsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "k", "s", time.Second)
	resp, err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsRateLimited() {
		t.Fatal("expected IsRateLimited()")
	}
}

func TestClient_StripsHopByHopAndAuthHeaders(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "k", "s", time.Second)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer client-token-do-not-forward")
	hdr.Set("Connection", "close")
	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, hdr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sawAuth == "Bearer client-token-do-not-forward" {
		t.Fatal("client Authorization header leaked to upstream")
	}
}
