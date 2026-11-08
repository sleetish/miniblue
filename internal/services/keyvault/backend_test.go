package keyvault

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moabukar/miniblue/internal/store"
)

func TestForwardingFixedAuthAndAzurePaths(t *testing.T) {
	var calls atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer operator-token" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Forwarded") != "" {
			t.Errorf("unexpected backend credentials/headers: %v", r.Header)
		}
		if strings.Contains(r.URL.Path, "keyvault") || r.URL.Query().Get("api-version") != "7.4" {
			t.Errorf("unexpected backend URL: %s", r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "secret=backend")
		w.Header().Set("X-Ms-Request-Id", "request-id")
		if r.Method == "DELETE" {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"code":"Forbidden","message":"canned backend failure"}}`))
			return
		}
		w.Write([]byte(`{"path":"` + r.URL.Path + `","body":` + func() string {
			if len(body) == 0 {
				return `null`
			}
			return string(body)
		}() + `}`))
	}))
	defer b.Close()
	s := store.New()
	h, err := NewHandlerWithConfig(s, Config{BackendURL: b.URL, BackendBearerToken: "operator-token"})
	if err != nil {
		t.Fatal(err)
	}
	routes := router(h)
	for _, tc := range []struct{ method, local, remote, body string }{
		{"GET", "", "/secrets", ""},
		{"PUT", "/storage", "/secrets/storage", `{"value":"connection"}`},
		{"GET", "/storage/v1", "/secrets/storage/v1", ""},
		{"GET", "/storage/versions", "/secrets/storage/versions", ""},
		{"GET", "/versions", "/secrets/versions", ""},
		{"DELETE", "/storage", "/secrets/storage", ""},
	} {
		req := httptest.NewRequest(tc.method, "/keyvault/ignored/secrets"+tc.local+"?api-version=7.4", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer inbound-token")
		req.Header.Set("X-Api-Key", "inbound-key")
		req.Header.Set("Cookie", "auth=inbound")
		req.Header.Set("Forwarded", "host=attacker")
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		if tc.method == "DELETE" {
			if w.Code != 403 || !strings.Contains(w.Body.String(), "canned backend failure") {
				t.Fatalf("error not preserved: %s", w.Body.String())
			}
		} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"path":"`+tc.remote+`"`) {
			t.Fatalf("forwarded response: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Ms-Request-Id") != "request-id" {
			t.Fatalf("response headers: %v", w.Header())
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("calls: %d", calls.Load())
	}
	if len(s.List()) != 0 {
		t.Fatal("backend operations mutated mock state")
	}
}

func TestBackendSafetyLimits(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "oversized", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "oversized":
					w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
				case "timeout":
					time.Sleep(50 * time.Millisecond)
					w.Write([]byte(`{}`))
				}
			}))
			defer b.Close()
			timeout := time.Second
			if mode == "timeout" {
				timeout = 10 * time.Millisecond
			}
			h, err := NewHandlerWithConfig(store.New(), Config{BackendURL: b.URL, BackendBearerToken: "fixed", BackendTimeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			routes := router(h)
			if got := request(routes, "GET", "/keyvault/test/secrets/key", ""); got.Code != 502 || got.Header().Get("Location") != "" {
				t.Fatalf("unsafe backend response: %d %s", got.Code, got.Body.String())
			}
			before := calls.Load()
			if got := request(routes, "PUT", "/keyvault/test/secrets/key", strings.Repeat("x", maxRequestBytes+1)); got.Code != 413 {
				t.Fatalf("oversized request: %d", got.Code)
			}
			if got := request(routes, "GET", "/keyvault/test/secrets/a%2Fb", ""); got.Code != 400 {
				t.Fatalf("invalid path: %d", got.Code)
			}
			if calls.Load() != before {
				t.Fatal("rejected requests reached backend")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestInvalidConfigAndDefaultOffline(t *testing.T) {
	for _, name := range []string{"KEYVAULT_BACKEND_URL", "KEYVAULT_BACKEND_BEARER_TOKEN", "KEYVAULT_BACKEND_API_KEY", "KEYVAULT_BACKEND_TIMEOUT"} {
		t.Setenv(name, "")
	}
	h, err := NewHandlerFromEnv(store.New())
	if err != nil || h.backend != nil {
		t.Fatalf("default config: %v", err)
	}
	for _, url := range []string{
		"://bad", "http://example.com", "https://user:pass@example.com", "https://example.com/keyvault",
		"https://example.com?query=1", "https://example.com#frag", "https://example.com#",
		"https://example.com?", "https://", "ftp://localhost", "https://localhost:99999",
		"https://localhost:0", "https://localhost:", "https://localhost:abc",
	} {
		if _, err := NewHandlerWithConfig(store.New(), Config{BackendURL: url, BackendBearerToken: "token"}); err == nil {
			t.Errorf("accepted URL %q", url)
		}
	}
	for _, c := range []Config{
		{BackendBearerToken: "token"},
		{BackendTimeout: time.Second},
		{BackendURL: "https://example.com"},
		{BackendURL: "https://example.com", BackendBearerToken: "token\r\nheader"},
		{BackendURL: "https://example.com", BackendBearerToken: "token\x01"},
		{BackendURL: "https://example.com", BackendBearerToken: "token", BackendTimeout: -time.Second},
		{BackendURL: "https://example.com", BackendBearerToken: "token", BackendTimeout: 2 * time.Minute},
	} {
		if _, err := NewHandlerWithConfig(store.New(), c); err == nil {
			t.Errorf("accepted invalid config")
		}
	}
	t.Setenv("KEYVAULT_BACKEND_API_KEY", "wrong")
	if _, err := NewHandlerFromEnv(store.New()); err == nil {
		t.Fatal("accepted API key")
	}
	t.Setenv("KEYVAULT_BACKEND_API_KEY", "")
	t.Setenv("KEYVAULT_BACKEND_TIMEOUT", "invalid")
	if _, err := NewHandlerFromEnv(store.New()); err == nil {
		t.Fatal("accepted timeout")
	}
	t.Setenv("KEYVAULT_BACKEND_TIMEOUT", "0s")
	if _, err := NewHandlerFromEnv(store.New()); err == nil {
		t.Fatal("accepted zero timeout")
	}
	t.Setenv("KEYVAULT_BACKEND_URL", "https://example.com")
	t.Setenv("KEYVAULT_BACKEND_BEARER_TOKEN", "operator-token")
	t.Setenv("KEYVAULT_BACKEND_TIMEOUT", "2m")
	if _, err := NewHandlerFromEnv(store.New()); err == nil {
		t.Fatal("accepted excessive timeout")
	}
	t.Setenv("KEYVAULT_BACKEND_TIMEOUT", "1s")
	configured, err := NewHandlerFromEnv(store.New())
	if err != nil || configured.backend == nil || configured.backend.client.Timeout != time.Second {
		t.Fatalf("valid env config: %v", err)
	}
	// The existing constructor must remain entirely offline even with backend env.
	local := NewHandler(store.New())
	if local.backend != nil {
		t.Fatal("legacy constructor enabled network")
	}
	secretResponse(t, request(router(local), "PUT", "/keyvault/test/secrets/key", `{"value":"local"}`))
}
