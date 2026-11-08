package tests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moabukar/miniblue/internal/server"
)

func clearAzureAIConfig(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SEARCH_BACKEND_URL", "SEARCH_BACKEND_API_KEY", "SEARCH_MOCK_RESPONSES",
		"OPENAI_BACKEND_URL", "OPENAI_BACKEND_API_KEY", "OPENAI_MOCK_RESPONSES",
	} {
		t.Setenv(key, "")
	}
}

func TestAzureAISearchIndexAndLexicalSearch(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/search/demo/indexes/products"
	resp := doRequest(t, http.MethodPut, base, `{"name":"products","fields":[{"name":"id","key":true}]}`)
	expectStatus(t, resp, 201)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, base+"/docs/index", `{"value":[{"@search.action":"upload","id":"a","name":"blue shoes","category":"shoes"},{"@search.action":"upload","id":"b","name":"red hat","category":"hats"}]}`)
	expectStatus(t, resp, 200)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, base+"/docs/search", `{"search":"blue shoes","filter":"category eq 'shoes'","queryType":"semantic"}`)
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	result := decodeJSON(t, resp)["value"].([]interface{})
	if len(result) != 1 {
		t.Fatalf("want one matching document, got %d", len(result))
	}
	doc := result[0].(map[string]interface{})
	if doc["id"] != "a" || doc["@search.score"] == nil || doc["@search.rerankerScore"] == nil {
		t.Fatalf("unexpected lexical/semantic result: %#v", doc)
	}
}

func TestAzureAISearchARMCRUD(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Search/searchServices"
	resp := doRequest(t, http.MethodPut, base+"/search-a", `{"location":"eastus","sku":{"name":"basic"}}`)
	expectStatus(t, resp, 201)
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, base+"/search-a", "")
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	if decodeJSON(t, resp)["type"] != "Microsoft.Search/searchServices" {
		t.Fatal("expected Search ARM resource type")
	}
}

func TestAzureAISearchMergeOrUploadRetainsExistingFields(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/search/demo/indexes/products"
	resp := doRequest(t, http.MethodPut, base, `{"name":"products"}`)
	expectStatus(t, resp, 201)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, base+"/docs/index", `{"value":[{"@search.action":"upload","id":"a","content":"benefits","planYear":2026},{"@search.action":"mergeOrUpload","id":"a","planYear":2027}]}`)
	expectStatus(t, resp, 200)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, base+"/docs/search", `{"search":"benefits","filter":"planYear eq 2027"}`)
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	values := decodeJSON(t, resp)["value"].([]interface{})
	if len(values) != 1 {
		t.Fatalf("mergeOrUpload discarded existing document fields: %v", values)
	}
}

func TestAzureOpenAIDataPlaneResponsesAreSynthetic(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	resp := doRequest(t, http.MethodPost, ts.URL+"/openai/deployments/embed/embeddings?api-version=2024-02-01", `{"input":"test"}`)
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	data := decodeJSON(t, resp)["data"].([]interface{})
	embedding := data[0].(map[string]interface{})["embedding"].([]interface{})
	if len(embedding) != 1536 {
		t.Fatalf("want 1536 synthetic dimensions, got %d", len(embedding))
	}
	resp = doRequest(t, http.MethodPost, ts.URL+"/openai/deployments/chat-model/chat/completions?api-version=2024-02-01", `{"messages":[{"role":"user","content":"hello"}]}`)
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	choices := decodeJSON(t, resp)["choices"].([]interface{})
	message := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if !strings.Contains(message["content"].(string), "hello") {
		t.Fatalf("unexpected deterministic chat response: %v", message["content"])
	}
}

func TestAzureOpenAIARMCRUD(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.CognitiveServices/accounts"
	resp := doRequest(t, http.MethodPut, base+"/account", `{"kind":"OpenAI","location":"eastus"}`)
	expectStatus(t, resp, 201)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPut, base+"/account/deployments/chat", `{"properties":{"model":{"name":"gpt-test"}}}`)
	expectStatus(t, resp, 201)
	if got := decodeJSON(t, resp)["id"]; got != "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.CognitiveServices/accounts/account/deployments/chat" {
		t.Fatalf("unexpected deployment resource ID: %v", got)
	}
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, base+"/account/deployments", "")
	defer resp.Body.Close()
	expectStatus(t, resp, 200)
	if len(decodeJSON(t, resp)["value"].([]interface{})) != 1 {
		t.Fatal("expected one deployment")
	}
}

func TestAzureAIMockOverrideCanReturn429(t *testing.T) {
	clearAzureAIConfig(t)
	t.Setenv("SEARCH_MOCK_RESPONSES", `{"POST /indexes/catalog/docs/search":{"status":429,"body":{"error":{"code":"TooManyRequests"}}}}`)
	t.Setenv("OPENAI_MOCK_RESPONSES", `{"POST /openai/deployments/embed/embeddings":{"status":500,"body":{"error":{"code":"SyntheticFailure"}}}}`)
	ts := setupServer()
	defer ts.Close()
	resp := doRequest(t, http.MethodPost, ts.URL+"/search/demo/indexes/catalog/docs/search", `{"search":"x"}`)
	expectStatus(t, resp, 429)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, ts.URL+"/openai/deployments/embed/embeddings?api-version=2024-02-01", `{"input":"x"}`)
	defer resp.Body.Close()
	expectStatus(t, resp, 500)
}

func TestAzureAIBackendIsFixedAndStripsInboundCredentials(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotCookie, gotAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth, gotCookie, gotAPIKey = r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("api-key")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer backend.Close()
	clearAzureAIConfig(t)
	t.Setenv("SEARCH_BACKEND_URL", backend.URL+"/prefix")
	t.Setenv("SEARCH_BACKEND_API_KEY", "operator-key")
	ts := setupServer()
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/search/demo/indexes/idx/docs/search?api-version=v1", strings.NewReader(`{"search":"x"}`))
	req.Header.Set("Authorization", "caller-secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("api-key", "caller-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp, 200)
	if gotPath != "/prefix/indexes/idx/docs/search" || gotQuery != "api-version=v1" {
		t.Fatalf("backend URL not preserved: %q?%s", gotPath, gotQuery)
	}
	if gotAuth != "" || gotCookie != "" || gotAPIKey != "operator-key" {
		t.Fatalf("unexpected forwarded credentials: authorization=%q cookie=%q api-key=%q", gotAuth, gotCookie, gotAPIKey)
	}
}

func TestAzureAIBackendRejectsRedirectAndOversizedResponse(t *testing.T) {
	for _, mode := range []string{"redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			clearAzureAIConfig(t)
			destinationCalls := 0
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destinationCalls++
			}))
			defer destination.Close()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, destination.URL, http.StatusFound)
				} else {
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
				}
			}))
			defer backend.Close()
			t.Setenv("OPENAI_BACKEND_URL", backend.URL)
			ts := setupServer()
			defer ts.Close()
			resp := doRequest(t, http.MethodPost, ts.URL+"/openai/deployments/chat/chat/completions", `{"messages":[{"role":"user","content":"test"}]}`)
			defer resp.Body.Close()
			expectStatus(t, resp, http.StatusBadGateway)
			if destinationCalls != 0 {
				t.Fatal("backend redirect was followed")
			}
		})
	}
}

func TestAzureAIOfflineLimitsAreExplicit(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/search/demo/indexes/benefits"
	resp := doRequest(t, http.MethodPost, base+"/docs/search", `{"search":"*"}`)
	expectStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPut, base, `{"name":"benefits","fields":[{"name":"id","key":true}]}`)
	expectStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	for _, test := range []struct{ path, body string }{
		{base + "/docs/search", `{"vectorQueries":[{"kind":"vector","vector":[0,1],"fields":"embedding"}]}`},
		{ts.URL + "/openai/deployments/chat/chat/completions", `{"messages":[{"role":"user","content":"test"}],"stream":true}`},
		{ts.URL + "/openai/deployments/embed/embeddings", `{"input":[]}`},
		{ts.URL + "/openai/deployments/embed/embeddings", `{"input":"text","dimensions":1537}`},
	} {
		resp := doRequest(t, http.MethodPost, test.path, test.body)
		expectStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
}
func TestAzureAIInvalidBackendConfigurationFailsStartup(t *testing.T) {
	clearAzureAIConfig(t)
	t.Setenv("OPENAI_BACKEND_URL", "file:///etc/passwd")
	if _, err := server.NewWithError(); err == nil {
		t.Fatal("expected invalid backend URL to fail startup")
	}
	t.Setenv("OPENAI_BACKEND_URL", "")
	t.Setenv("SEARCH_MOCK_RESPONSES", "not-json")
	if _, err := server.NewWithError(); err == nil {
		t.Fatal("expected malformed mock configuration to fail startup")
	}
}

func TestAzureAIServicesAppearInHealth(t *testing.T) {
	clearAzureAIConfig(t)
	t.Setenv("SERVICES", "search,openai")
	ts := setupServer()
	defer ts.Close()
	resp := doRequest(t, http.MethodGet, ts.URL+"/health", "")
	defer resp.Body.Close()
	services := decodeJSON(t, resp)["services"].([]interface{})
	found := map[string]bool{}
	for _, service := range services {
		found[service.(string)] = true
	}
	if !found["search"] || !found["openai"] {
		t.Fatalf("health services do not list enabled Azure AI services: %v", services)
	}
}

func TestAzureAIResourceGroupDeletionCleansUpARMResources(t *testing.T) {
	clearAzureAIConfig(t)
	ts := setupServer()
	defer ts.Close()
	rg := ts.URL + "/subscriptions/sub1/resourcegroups/ai-rg?api-version=2020-06-01"
	resp := doRequest(t, http.MethodPut, rg, `{"location":"eastus"}`)
	expectStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	base := ts.URL + "/subscriptions/sub1/resourceGroups/ai-rg/providers/"
	search := base + "Microsoft.Search/searchServices/search"
	account := base + "Microsoft.CognitiveServices/accounts/account"
	for _, entry := range []struct{ url, body string }{
		{search, `{"location":"eastus"}`},
		{account, `{"kind":"OpenAI"}`},
		{account + "/deployments/embed", `{}`},
	} {
		resp = doRequest(t, http.MethodPut, entry.url, entry.body)
		expectStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}
	resp = doRequest(t, http.MethodDelete, rg, "")
	expectStatus(t, resp, http.StatusAccepted)
	resp.Body.Close()
	for _, path := range []string{search, account, account + "/deployments/embed"} {
		resp = doRequest(t, http.MethodGet, path, "")
		expectStatus(t, resp, http.StatusNotFound)
		resp.Body.Close()
	}
}
