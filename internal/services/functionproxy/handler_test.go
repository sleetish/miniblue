package functionproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"FUNCTION_BACKEND_URL", "FUNCTION_BACKEND_API_KEY", "FUNCTION_BACKEND_TIMEOUT", "FUNCTION_MOCK_RESPONSES"} {
		t.Setenv(key, "")
	}
}

func newTestRouter(t *testing.T) *chi.Mux {
	t.Helper()
	h, err := NewHandler(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.Register(r)
	return r
}

func invoke(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestOfflineAndMocks(t *testing.T) {
	clearEnv(t)
	r := newTestRouter(t)
	if got := invoke(r, "POST", "/api/Benny", "{}"); got.Code != 501 || !strings.Contains(got.Body.String(), "FunctionInvocationNotConfigured") {
		t.Fatalf("unexpected offline response: %d %s", got.Code, got.Body.String())
	}
	t.Setenv("FUNCTION_MOCK_RESPONSES", `{
		"POST /api/Benny":{"status":202,"body":{"accepted":true}},
		"POST /api/Benny/*":{"status":503,"body":{"error":"mock failure"}},
		"POST /api/Benny/exact":{"status":200,"body":"exact"},
		"HEAD /api/Benny":{"status":200,"body":"ignored"},
		"DELETE /api/Benny":{"status":204,"body":null}
	}`)
	r = newTestRouter(t)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/Benny?code=ignored", `{"accepted":true}`, 202},
		{"POST", "/api/Benny/path/deep", `{"error":"mock failure"}`, 503},
		{"POST", "/api/Benny/", `{"error":"mock failure"}`, 503},
		{"POST", "/api/Benny/exact", `"exact"`, 200},
		{"HEAD", "/api/Benny", "", 200},
		{"DELETE", "/api/Benny", "", 204},
		{"GET", "/api/Benny", "FunctionInvocationNotConfigured", 501},
		{"POST", "/api/benny", "FunctionInvocationNotConfigured", 501},
		{"TRACE", "/api/Benny", "MethodNotAllowed", 405},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			got := invoke(r, tc.method, tc.path, "")
			if got.Code != tc.status || !strings.Contains(got.Body.String(), tc.body) || (tc.body == "" && got.Body.Len() != 0) {
				t.Fatalf("got %d %s", got.Code, got.Body.String())
			}
		})
	}
}

func TestPassthroughCredentialsAndOverrides(t *testing.T) {
	clearEnv(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		data, _ := io.ReadAll(r.Body)
		if r.Method != "PATCH" || r.URL.Path != "/base/api/Benny/a/b" || string(data) != `{"input":"synthetic"}` {
			t.Errorf("incorrect forwarded method/path/body: %s %s %s", r.Method, r.URL.Path, data)
		}
		if r.URL.Query().Encode() != "backend_url=http%3A%2F%2Fcaller.invalid&message=hello&message=world" {
			t.Errorf("incorrect filtered query: %s", r.URL.RawQuery)
		}
		for _, key := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key", "X-Forwarded-Host", "X-Forwarded-Authorization"} {
			if r.Header.Get(key) != "" {
				t.Errorf("credential header forwarded: %s", key)
			}
		}
		if r.Header.Get("X-Functions-Key") != "synthetic-backend-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("backend headers: %v", r.Header)
		}
		w.Header().Set("Set-Cookie", "secret=backend")
		w.Header().Set("X-Functions-Key", "backend-key")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(422)
		_, _ = w.Write([]byte("real backend error"))
	}))
	defer backend.Close()
	t.Setenv("FUNCTION_BACKEND_URL", backend.URL+"/base/")
	t.Setenv("FUNCTION_BACKEND_API_KEY", "synthetic-backend-key")
	t.Setenv("FUNCTION_MOCK_RESPONSES", `{"PATCH /api/Benny/local":{"status":200,"body":"local"}}`)
	r := newTestRouter(t)
	req := httptest.NewRequest("PATCH", "/api/Benny/a/b?CoDe=client&CODE=other&x-functions-key=caller&access_token=bearer&API-KEY=caller&message=hello&message=world&backend_url=http%3A%2F%2Fcaller.invalid", strings.NewReader(`{"input":"synthetic"}`))
	for _, key := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key", "X-Functions-Key", "X-Forwarded-Host", "X-Forwarded-Authorization"} {
		req.Header.Set(key, "caller-secret")
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 422 || rec.Body.String() != "real backend error" || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("X-Functions-Key") != "" {
		t.Fatalf("incorrect response: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = invoke(r, "PATCH", "/api/Benny/local", "")
	if rec.Code != 200 || rec.Body.String() != `"local"` || calls.Load() != 1 {
		t.Fatalf("mock did not override backend: %d calls %d", rec.Code, calls.Load())
	}
}

func TestBoundsRedirectTimeoutAndPaths(t *testing.T) {
	clearEnv(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/Benny/redirect":
			http.Redirect(w, r, "/api/Benny/destination", 302)
		case "/api/Benny/large":
			_, _ = w.Write([]byte(strings.Repeat("x", maxBodySize+1)))
		case "/api/Benny/slow":
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer backend.Close()
	t.Setenv("FUNCTION_BACKEND_URL", backend.URL)
	t.Setenv("FUNCTION_BACKEND_TIMEOUT", "100ms")
	r := newTestRouter(t)
	for _, tc := range []struct {
		path, body, code string
		status           int
	}{
		{"/api/Benny", strings.Repeat("x", maxBodySize+1), "RequestBodyTooLarge", 413},
		{"/api/Benny/redirect", "", "FunctionBackendRedirectRejected", 502},
		{"/api/Benny/large", "", "FunctionBackendResponseInvalid", 502},
		{"/api/Benny/slow", "", "FunctionBackendTimeout", 504},
		{"/api/Benny/../other", "", "InvalidRequestContent", 400},
		{"/api/Benny/%2e%2e/other", "", "InvalidRequestContent", 400},
		{"/api/Benny/%252e%252e/other", "", "InvalidRequestContent", 400},
		{"/api/Benny/a%5cb", "", "InvalidRequestContent", 400},
		{"/api/Benny?code=%zz", "", "InvalidRequestContent", 400},
	} {
		got := invoke(r, "POST", tc.path, tc.body)
		if got.Code != tc.status || !strings.Contains(got.Body.String(), tc.code) {
			t.Errorf("%s: got %d %s", tc.path, got.Code, got.Body.String())
		}
	}
	if calls.Load() != 3 {
		t.Errorf("unexpected backend calls (redirect/input/path protection): %d", calls.Load())
	}
	t.Setenv("FUNCTION_BACKEND_URL", "")
	r = newTestRouter(t)
	if got := invoke(r, "POST", "/api/Benny", strings.Repeat("x", maxBodySize+1)); got.Code != 413 {
		t.Errorf("offline oversized body: %d", got.Code)
	}
}

func TestStartupValidation(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"FUNCTION_BACKEND_URL", "https://user:pass@example.com"},
		{"FUNCTION_BACKEND_URL", "https://example.com?code=secret"},
		{"FUNCTION_BACKEND_URL", "https://example.com/#fragment"},
		{"FUNCTION_BACKEND_URL", "https://example.com/%2fapi"},
		{"FUNCTION_BACKEND_URL", "https://example.com/../api"},
		{"FUNCTION_BACKEND_URL", "file:///etc/passwd"},
		{"FUNCTION_BACKEND_API_KEY", "without-backend"},
		{"FUNCTION_BACKEND_TIMEOUT", "0"},
		{"FUNCTION_BACKEND_TIMEOUT", "2m"},
		{"FUNCTION_MOCK_RESPONSES", "null"},
		{"FUNCTION_MOCK_RESPONSES", "{} {}"},
		{"FUNCTION_MOCK_RESPONSES", `{"GET /api/Benny":{"status":200,"body":"ok","typo":true}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"get /api/Benny":{"status":200,"body":"ok"}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"POST /api/Benny?code=x":{"status":200,"body":"ok"}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"POST /api/Benny":{"status":199,"body":"ok"}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"POST /api/Benny":{"status":200}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"POST /api/Benny":{"status":204,"body":"bad"}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"GET /api/Benny":{"status":200,"body":"first"},"GET /api/Benny":{"status":500,"body":"duplicate"}}`},
		{"FUNCTION_MOCK_RESPONSES", `{"GET /api/Benny/sub/*":{"status":200,"body":"unsupported wildcard"}}`},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := NewHandler(nil); err == nil {
				t.Fatal("expected startup error")
			}
		})
	}
	clearEnv(t)
	t.Setenv("FUNCTION_BACKEND_URL", "http://localhost:7071")
	t.Setenv("FUNCTION_BACKEND_API_KEY", "bad\r\nkey")
	if _, err := NewHandler(nil); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestNoCallerFunctionKeyWithoutBackendKey(t *testing.T) {
	clearEnv(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Functions-Key") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.RawQuery != "keep=value" {
			t.Errorf("credentials leaked: %v %s", r.Header, r.URL.RawQuery)
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()
	t.Setenv("FUNCTION_BACKEND_URL", backend.URL)
	r := newTestRouter(t)
	req := httptest.NewRequest("GET", "/api/Benny?cOdE=secret&keep=value", nil)
	req.Header.Set("x-functions-key", "caller")
	req.Header.Set("Authorization", "Bearer caller")
	req.Header.Set("Cookie", "session=caller")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
}
