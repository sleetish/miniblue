package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/moabukar/miniblue/internal/store"
)

func router(t *testing.T, backend store.Backend) *chi.Mux {
	t.Helper()
	h, err := NewHandler(backend)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.Register(r)
	return r
}

func request(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func status(t *testing.T, got *httptest.ResponseRecorder, want int) {
	t.Helper()
	if got.Code != want {
		t.Fatalf("status = %d, want %d; body %s", got.Code, want, got.Body.String())
	}
}

func TestResourceCRUDAndIsolation(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	r := router(t, store.NewMemoryBackend())
	types := []string{"Microsoft.Insights/components", "Microsoft.OperationalInsights/workspaces", "Microsoft.Insights/actionGroups", "Microsoft.Insights/metricAlerts", "Microsoft.Insights/workbooks"}
	for _, typ := range types {
		base := "/subscriptions/sub/resourceGroups/rg/providers/" + typ
		res := base + "/test"
		status(t, request(t, r, "PUT", res, `{"location":"eastus","tags":{"owner":"Benny"},"properties":{"foo":"bar"}}`), 201)
		w := request(t, r, "GET", res, "")
		status(t, w, 200)
		var got map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["id"] != res || got["name"] != "test" || got["type"] != typ || got["properties"].(map[string]interface{})["foo"] != "bar" {
			t.Fatalf("unexpected resource: %#v", got)
		}
		status(t, request(t, r, "PUT", res, `{"properties":{"foo":"updated"}}`), 200)
		w = request(t, r, "GET", base, "")
		if !strings.Contains(w.Body.String(), "updated") {
			t.Fatalf("list missing resource: %s", w.Body.String())
		}
		status(t, request(t, r, "GET", strings.Replace(res, "/rg/", "/other/", 1), ""), 404)
		status(t, request(t, r, "GET", strings.Replace(res, "/sub/", "/other/", 1), ""), 404)
		status(t, request(t, r, "PUT", res, `{"properties":null}`), 400)
		status(t, request(t, r, "PUT", res, `null`), 400)
		status(t, request(t, r, "DELETE", res, ""), 204)
		status(t, request(t, r, "GET", res, ""), 404)
		status(t, request(t, r, "DELETE", res, ""), 404)
	}
}

func TestCleanupResourcesInRG(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	backend := store.NewMemoryBackend()
	r := router(t, backend)
	for _, typ := range resourceTypes {
		for _, scope := range []string{"sub/rg", "sub/rg2", "sub2/rg"} {
			parts := strings.Split(scope, "/")
			base := "/subscriptions/" + parts[0] + "/resourceGroups/" + parts[1] + "/providers/" + typ
			for _, name := range []string{"a", "b"} {
				status(t, request(t, r, "PUT", base+"/"+name, `{}`), 201)
			}
		}
	}
	status(t, request(t, r, "POST", "/monitor/records", recordJSON("rg", "2026-01-01T00:00:00Z", "outcome")), 201)
	if err := backend.Set("other:sub:rg:a", "keep"); err != nil {
		t.Fatal(err)
	}
	CleanupResourcesInRG(backend, "SUB", "RG")
	CleanupResourcesInRG(backend, "sub", "rg")
	for _, typ := range resourceTypes {
		for _, scope := range []string{"sub/rg", "sub/rg2", "sub2/rg"} {
			parts := strings.Split(scope, "/")
			base := "/subscriptions/" + parts[0] + "/resourceGroups/" + parts[1] + "/providers/" + typ
			want := 200
			if scope == "sub/rg" {
				want = 404
			}
			for _, name := range []string{"a", "b"} {
				status(t, request(t, r, "GET", base+"/"+name, ""), want)
			}
		}
	}
	if _, ok := backend.Get("other:sub:rg:a"); !ok {
		t.Fatal("unrelated key removed")
	}
	w := request(t, r, "GET", "/monitor/records?conversation_id=rg", "")
	var list struct {
		Value []record `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Value) != 1 {
		t.Fatalf("telemetry removed: %s (%v)", w.Body.String(), err)
	}
}

func recordJSON(conv, timestamp, typ string) string {
	b, _ := json.Marshal(map[string]interface{}{"conversation_id": conv, "timestamp": timestamp, "type": typ, "data": map[string]interface{}{"result": "ok"}})
	return string(b)
}

func TestRecordsAndDeletion(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	r := router(t, store.NewMemoryBackend())
	old := "2026-01-01T00:00:00Z"
	cutoff := "2026-01-08T00:00:00Z"
	for _, v := range []struct{ conv, time, typ string }{
		{"a", old, "telemetry"}, {"a", cutoff, "outcome"}, {"a:b", old, "telemetry"}, {"A", old, "outcome"},
	} {
		status(t, request(t, r, "POST", "/monitor/records", recordJSON(v.conv, v.time, v.typ)), 201)
	}
	w := request(t, r, "GET", "/monitor/records?conversation_id=a", "")
	status(t, w, 200)
	var list struct {
		Value []record `json:"value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Value) != 2 {
		t.Fatalf("list: %s, error %v", w.Body.String(), err)
	}
	status(t, request(t, r, "DELETE", "/monitor/records?conversation_id=a&before="+url.QueryEscape(cutoff), ""), 204)
	w = request(t, r, "GET", "/monitor/records?conversation_id=a", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Value) != 1 || list.Value[0].Timestamp != cutoff {
		t.Fatalf("cutoff result: %s", w.Body.String())
	}
	for _, conv := range []string{"a:b", "A"} {
		w = request(t, r, "GET", "/monitor/records?conversation_id="+url.QueryEscape(conv), "")
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Value) != 1 {
			t.Fatalf("isolated result: %s", w.Body.String())
		}
	}
	status(t, request(t, r, "DELETE", "/monitor/records?conversation_id=a", ""), 204)
	w = request(t, r, "GET", "/monitor/records?conversation_id=a", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Value) != 0 {
		t.Fatalf("delete result: %s", w.Body.String())
	}
}

func TestValidation(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	r := router(t, store.NewMemoryBackend())
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"conversation_id":"a","timestamp":"bad","type":"telemetry","data":{}}`,
		`{"conversation_id":"a","timestamp":"2026-01-01T00:00:00Z","type":"alert","data":{}}`,
		`{"conversation_id":"a","timestamp":"2026-01-01T00:00:00Z","type":"outcome","data":null}`,
		`{"conversation_id":"a","timestamp":"2026-01-01T00:00:00Z","type":"outcome","data":{},"id":"x"}`,
		`{"conversation_id":"a","timestamp":"2026-01-01T00:00:00Z","type":"outcome","data":{}} {}`,
		strings.Repeat("x", maxBody+1),
	} {
		status(t, request(t, r, "POST", "/monitor/records", body), 400)
	}
	big := fmt.Sprintf(`{"conversation_id":"a","timestamp":"2026-01-01T00:00:00Z","type":"telemetry","data":{"x":"%s"}}`, strings.Repeat("a", maxRecordBody))
	status(t, request(t, r, "POST", "/monitor/records", big), 413)
	for _, path := range []string{"/monitor/records", "/monitor/records?conversation_id=a&conversation_id=b", "/monitor/records?conversation_id=a&api-key=secret", "/monitor/records?conversation_id=a;bad", "/monitor/records?conversation_id=a&before=bad"} {
		status(t, request(t, r, "DELETE", path, ""), 400)
	}
	status(t, request(t, r, "POST", "/monitor/records?api-key=x", recordJSON("a", "2026-01-01T00:00:00Z", "outcome")), 400)
	status(t, request(t, r, "GET", "/monitor/records?conversation_id=a", "unexpected"), 400)
	status(t, request(t, r, "DELETE", "/monitor/records?conversation_id=a", strings.Repeat("x", maxBody+1)), 400)
}

func TestBackendForwardingSecurity(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_API_KEY", "operator-key")
	hitRedirect := false
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hitRedirect = true }))
	defer redirect.Close()
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/prefix/monitor/records" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("api-key") != "operator-key" || r.Header.Get("X-Secret") != "" {
			t.Errorf("unsafe forwarded request: path=%s headers=%v", r.URL.Path, r.Header)
		}
		switch r.URL.Query().Get("conversation_id") {
		case "redirect":
			http.Redirect(w, r, redirect.URL, 302)
		case "large":
			fmt.Fprint(w, strings.Repeat("x", maxBody+1))
		default:
			w.Header().Set("Set-Cookie", "secret=x")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			fmt.Fprint(w, `{"from":"backend"}`)
		}
	}))
	defer upstream.Close()
	t.Setenv("MONITOR_BACKEND_URL", upstream.URL+"/prefix")
	r := router(t, store.NewMemoryBackend())
	req := httptest.NewRequest("GET", "/monitor/records?conversation_id=a", nil)
	req.Header.Set("Authorization", "Bearer caller")
	req.Header.Set("Cookie", "caller=secret")
	req.Header.Set("api-key", "caller")
	req.Header.Set("X-Secret", "caller")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	status(t, w, 202)
	if w.Header().Get("Set-Cookie") != "" || !strings.Contains(w.Body.String(), "backend") {
		t.Fatalf("unexpected response: %v %s", w.Header(), w.Body.String())
	}
	status(t, request(t, r, "GET", "/monitor/records?conversation_id=redirect", ""), 502)
	status(t, request(t, r, "GET", "/monitor/records?conversation_id=large", ""), 502)
	status(t, request(t, r, "POST", "/monitor/records", recordJSON("a", "2026-01-01T00:00:00Z", "telemetry")), 202)
	status(t, request(t, r, "DELETE", "/monitor/records?conversation_id=a&before=2026-01-01T00%3A00%3A00Z", ""), 202)
	status(t, request(t, r, "GET", "/monitor/records?conversation_id=a&api-key=caller", ""), 400)
	status(t, request(t, r, "PUT", "/subscriptions/s/resourceGroups/r/providers/Microsoft.Insights/components/a", `{}`), 201)
	if hitRedirect || calls != 5 {
		t.Fatalf("redirect followed=%v calls=%d", hitRedirect, calls)
	}
}

func TestConfigurationAndTimeout(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	for _, raw := range []string{"ftp://localhost", "http://user:pass@localhost", "http://localhost?q=1", "http://localhost#fragment", "http://localhost?"} {
		t.Setenv("MONITOR_BACKEND_URL", raw)
		if _, err := NewHandler(store.NewMemoryBackend()); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "key")
	if _, err := NewHandler(store.NewMemoryBackend()); err == nil {
		t.Fatal("accepted key without backend")
	}
	t.Setenv("MONITOR_BACKEND_URL", "http://127.0.0.1:1")
	h, err := NewHandler(store.NewMemoryBackend())
	if err != nil {
		t.Fatal(err)
	}
	if h.client.Timeout != 10*time.Second {
		t.Fatalf("timeout: %s", h.client.Timeout)
	}
	r := chi.NewRouter()
	h.Register(r)
	status(t, request(t, r, "GET", "/monitor/records?conversation_id=a", ""), 502)
}

func TestRecordLimits(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	backend := store.NewMemoryBackend()
	r := router(t, backend)
	for i := 0; i < maxRecords; i++ {
		if err := backend.Set(fmt.Sprintf("%s%d", recordPrefix("full"), i), record{}); err != nil {
			t.Fatal(err)
		}
	}
	status(t, request(t, r, "POST", "/monitor/records", recordJSON("full", "2026-01-01T00:00:00Z", "outcome")), 409)
	for i := 0; i < 80; i++ {
		status(t, request(t, r, "POST", "/monitor/records", fmt.Sprintf(`{"conversation_id":"large","timestamp":"2026-01-01T00:00:00Z","type":"telemetry","data":{"x":"%s"}}`, strings.Repeat("a", 14000))), 201)
	}
	status(t, request(t, r, "GET", "/monitor/records?conversation_id=large", ""), 413)
}

type failingBackend struct{ store.Backend }

func (f failingBackend) Set(string, interface{}) error { return fmt.Errorf("storage unavailable") }

func TestStorageWriteFailure(t *testing.T) {
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	r := router(t, failingBackend{store.NewMemoryBackend()})
	status(t, request(t, r, "PUT", "/subscriptions/s/resourceGroups/r/providers/Microsoft.Insights/components/a", `{}`), 500)
	status(t, request(t, r, "POST", "/monitor/records", recordJSON("a", "2026-01-01T00:00:00Z", "outcome")), 500)
}
