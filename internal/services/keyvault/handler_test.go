package keyvault

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/moabukar/miniblue/internal/store"
)

func router(h *Handler) http.Handler {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func secretResponse(t *testing.T, w *httptest.ResponseRecorder) Secret {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var s Secret
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRotationAndDeletion(t *testing.T) {
	s := store.New()
	h := router(NewHandler(s))
	base := "/keyvault/test/secrets/function-key"
	first := request(h, "PUT", base, `{"value":"first"}`)
	if got := secretResponse(t, first); got.Value != "first" || got.ID != "https://test.vault.azure.net/secrets/function-key" || got.Attributes["enabled"] != "true" {
		t.Fatalf("legacy response changed: %+v", got)
	}
	v1 := first.Header().Get("X-Miniblue-Secret-Version")
	second := request(h, "PUT", base, `{"value":"rotated"}`)
	v2 := second.Header().Get("X-Miniblue-Secret-Version")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Fatalf("invalid versions %q %q", v1, v2)
	}
	// A new handler shares persisted history, not handler-local secret state.
	h = router(NewHandler(s))
	for _, tc := range []struct{ path, value, suffix string }{
		{base, "rotated", "function-key"},
		{base + "/" + v1, "first", v1},
		{base + "/" + v2, "rotated", v2},
	} {
		got := secretResponse(t, request(h, "GET", tc.path, ""))
		if got.Value != tc.value || !strings.HasSuffix(got.ID, "/"+tc.suffix) {
			t.Fatalf("%s: %+v", tc.path, got)
		}
	}
	versions := request(h, "GET", base+"/versions", "")
	var list struct {
		Value []map[string]any `json:"value"`
	}
	if err := json.Unmarshal(versions.Body.Bytes(), &list); err != nil || len(list.Value) != 2 {
		t.Fatalf("version list: %s, %v", versions.Body.String(), err)
	}
	for _, item := range list.Value {
		if _, exists := item["value"]; exists {
			t.Fatal("version listing leaked value")
		}
	}
	listed := request(h, "GET", "/keyvault/test/secrets", "")
	if strings.Contains(listed.Body.String(), "rotated") || strings.Contains(listed.Body.String(), v2) {
		t.Fatalf("latest metadata listing: %s", listed.Body.String())
	}
	if got := request(h, "GET", "/keyvault/other/secrets/function-key/"+v1, ""); got.Code != 404 {
		t.Fatalf("vault isolation: %d", got.Code)
	}
	if got := request(h, "GET", base+"/unknown", ""); got.Code != 404 {
		t.Fatalf("unknown version: %d", got.Code)
	}
	if got := request(h, "DELETE", base, ""); got.Code != 200 {
		t.Fatalf("delete: %d", got.Code)
	}
	for _, path := range []string{base, base + "/" + v1, base + "/" + v2} {
		if got := request(h, "GET", path, ""); got.Code != 404 {
			t.Fatalf("deleted version %s: %d", path, got.Code)
		}
	}
	if got := request(h, "GET", base+"/versions", ""); got.Body.String() != "{\"value\":[]}\n" {
		t.Fatalf("deleted history: %s", got.Body.String())
	}
	fresh := request(h, "PUT", base, `{"value":"new"}`)
	if fresh.Code != 200 || fresh.Header().Get("X-Miniblue-Secret-Version") == v2 {
		t.Fatal("recreation reused deleted version")
	}
}

func TestLegacyAndJSONDecodedStore(t *testing.T) {
	s := store.New()
	s.Set("kv:test:old", map[string]any{
		"id":    "https://test.vault.azure.net/secrets/old",
		"value": "old-value", "attributes": map[string]any{"enabled": "true"},
	})
	h := router(NewHandler(s))
	if got := secretResponse(t, request(h, "GET", "/keyvault/test/secrets/old", "")); got.Value != "old-value" {
		t.Fatalf("legacy: %+v", got)
	}
	updated := request(h, "PUT", "/keyvault/test/secrets/old", `{"value":"new"}`)
	secretResponse(t, updated)
	v, _ := s.Get("kv:test:old")
	data, _ := json.Marshal(v)
	var decoded any
	json.Unmarshal(data, &decoded)
	s.Set("kv:test:old", decoded)
	if got := secretResponse(t, request(h, "GET", "/keyvault/test/secrets/old/"+updated.Header().Get("X-Miniblue-Secret-Version"), "")); got.Value != "new" {
		t.Fatalf("JSON roundtrip: %+v", got)
	}
	listed := request(h, "GET", "/keyvault/test/secrets", "")
	if !strings.Contains(listed.Body.String(), "/secrets/old") {
		t.Fatalf("JSON roundtrip list: %s", listed.Body.String())
	}
}

func TestInvalidMockBodyDoesNotRotate(t *testing.T) {
	h := router(NewHandler(store.New()))
	base := "/keyvault/test/secrets/storage-connection"
	secretResponse(t, request(h, "PUT", base, `{"value":"initial"}`))
	for _, body := range []string{`{`, `{"value":"bad"} {}`, strings.Repeat("x", maxRequestBytes+1)} {
		if got := request(h, "PUT", base, body); got.Code != 400 {
			t.Fatalf("invalid body: %d", got.Code)
		}
	}

	if got := secretResponse(t, request(h, "GET", base, "")); got.Value != "initial" {
		t.Fatal("invalid request changed secret")
	}
}

func TestCorruptStoredSecretsReturnErrors(t *testing.T) {
	const id = "https://test.vault.azure.net/secrets/key"
	valid := Secret{ID: id + "/v1", Value: "sensitive-value"}
	cases := []struct {
		name  string
		value any
	}{
		{"unencodable", make(chan int)},
		{"nil", nil},
		{"scalar", "sensitive-value"},
		{"array", []any{}},
		{"empty-object", map[string]any{}},
		{"empty-history", secretHistory{}},
		{"malformed-history", map[string]any{"versions": "sensitive-value"}},
		{"invalid-version", secretHistory{Versions: []Secret{{ID: "bad", Value: "sensitive-value"}}}},
		{"missing-version", secretHistory{Versions: []Secret{{ID: id + "/", Value: "sensitive-value"}}}},
		{"duplicate-version", secretHistory{Versions: []Secret{valid, valid}}},
		{"wrong-secret", secretHistory{Versions: []Secret{{ID: id + "-other/v1", Value: "sensitive-value"}}}},
		{"missing-value", map[string]any{"id": id}},
		{"null-value", map[string]any{"id": id, "value": nil}},
		{"invalid-attributes", map[string]any{"id": id, "value": "sensitive-value", "attributes": 123}},
		{"invalid-legacy-id", Secret{ID: "bad", Value: "sensitive-value"}},
		{"no-legacy-fallback", map[string]any{"versions": nil, "id": id, "value": "sensitive-value"}},
		{"invalid-version-value", map[string]any{"versions": []any{map[string]any{"id": id + "/v1", "value": 123}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := store.New()
			key := "kv:test:key"
			s.Set(key, tc.value)
			h := NewHandler(s)
			if _, err := h.history(key); err == nil {
				t.Fatal("history did not report corruption")
			}
			routes := router(h)
			for _, op := range []struct{ method, path, body string }{
				{"GET", "/keyvault/test/secrets/key", ""},
				{"GET", "/keyvault/test/secrets/key/v1", ""},
				{"GET", "/keyvault/test/secrets/key/versions", ""},
				{"GET", "/keyvault/test/secrets", ""},
				{"PUT", "/keyvault/test/secrets/key", `{"value":"replacement"}`},
			} {
				w := request(routes, op.method, op.path, op.body)
				if w.Code != 500 || !strings.Contains(w.Body.String(), `"code":"CorruptSecretState"`) || strings.Contains(w.Body.String(), "sensitive-value") {
					t.Fatalf("%s %s: %d %s", op.method, op.path, w.Code, w.Body.String())
				}
				stored, exists := s.Get(key)
				if !exists || !reflect.DeepEqual(stored, tc.value) {
					t.Fatal("corruption was overwritten")
				}
			}
			// Deletion intentionally remains a way to remove corrupted records.
			if w := request(routes, "DELETE", "/keyvault/test/secrets/key", ""); w.Code != 200 || s.Exists(key) {
				t.Fatal("could not delete corrupted record")
			}
			if w := request(routes, "GET", "/keyvault/test/secrets/key", ""); w.Code != 404 {
				t.Fatalf("missing secret changed behavior: %d", w.Code)
			}
			secretResponse(t, request(routes, "PUT", "/keyvault/test/secrets/key", `{"value":""}`))
		})
	}
}

func TestConcurrentRotationKeepsAllVersions(t *testing.T) {
	h := router(NewHandler(store.New()))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			got := request(h, "PUT", "/keyvault/test/secrets/key", `{"value":"same"}`)
			if got.Code != 200 {
				t.Errorf("concurrent write: %d", got.Code)
			}
		})
	}
	wg.Wait()
	got := request(h, "GET", "/keyvault/test/secrets/key/versions", "")
	var list struct {
		Value []map[string]any `json:"value"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &list); err != nil || len(list.Value) != 16 {
		t.Fatalf("lost concurrent versions: %s, %v", got.Body.String(), err)
	}
	seen := make(map[string]bool)
	for _, item := range list.Value {
		id := item["id"].(string)
		if seen[id] {
			t.Fatal("duplicate version")
		}
		seen[id] = true
	}
}
