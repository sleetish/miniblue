package tests

import (
	"net/http"
	"strings"
	"testing"

	"github.com/moabukar/miniblue/internal/server"
)

func TestBennyFunctionsMockAndConfiguration(t *testing.T) {
	t.Setenv("SERVICES", "sites")
	t.Setenv("FUNCTION_BACKEND_URL", "")
	t.Setenv("FUNCTION_BACKEND_API_KEY", "")
	t.Setenv("FUNCTION_MOCK_RESPONSES", `{"POST /api/benny/*":{"status":200,"body":{"status":"fixture"}}}`)
	ts := setupServer()
	defer ts.Close()

	resp := doRequest(t, http.MethodPost, ts.URL+"/api/benny/start?code=client-secret", `{}`)
	expectStatus(t, resp, http.StatusOK)
	if got := decodeJSON(t, resp)["status"]; got != "fixture" {
		t.Fatalf("unexpected function mock response: %v", got)
	}
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, ts.URL+"/api/retrieve", `{}`)
	expectStatus(t, resp, http.StatusNotImplemented)
	resp.Body.Close()

	site := ts.URL + "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Web/sites/benny"
	resp = doRequest(t, http.MethodPut, site, `{"location":"eastus"}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPut, site+"/config/appSettings", `{"properties":{"STORAGE":"synthetic"}}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doRequest(t, http.MethodPost, site+"/config/appSettings/list", `{}`)
	expectStatus(t, resp, http.StatusOK)
	if props := decodeJSON(t, resp)["properties"].(map[string]interface{}); props["STORAGE"] != "synthetic" {
		t.Fatalf("configuration was not retained: %v", props)
	}
	resp.Body.Close()
}

func TestBennyMonitorRecordsAndConfiguration(t *testing.T) {
	t.Setenv("SERVICES", "monitor")
	t.Setenv("MONITOR_BACKEND_URL", "")
	t.Setenv("MONITOR_BACKEND_API_KEY", "")
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Insights/metricAlerts/high-errors"
	resp := doRequest(t, http.MethodPut, base, `{"location":"global","properties":{"description":"fixture"}}`)
	expectStatus(t, resp, http.StatusCreated)
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, base, "")
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	records := ts.URL + "/monitor/records"
	for _, timestamp := range []string{"2026-01-01T00:00:00Z", "2026-01-09T00:00:00Z"} {
		resp = doRequest(t, http.MethodPost, records, `{"conversation_id":"test-id","timestamp":"`+timestamp+`","type":"outcome","data":{"milestone":"started"}}`)
		expectStatus(t, resp, http.StatusCreated)
		resp.Body.Close()
	}
	resp = doRequest(t, http.MethodDelete, records+"?conversation_id=test-id&before=2026-01-08T00%3A00%3A00Z", "")
	expectStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, records+"?conversation_id=test-id", "")
	expectStatus(t, resp, http.StatusOK)
	if values := decodeJSON(t, resp)["value"].([]interface{}); len(values) != 1 {
		t.Fatalf("expected one record after cutoff deletion: %v", values)
	}
	resp.Body.Close()
}

func TestBennySecretVersionRotation(t *testing.T) {
	t.Setenv("SERVICES", "keyvault")
	t.Setenv("KEYVAULT_BACKEND_URL", "")
	t.Setenv("KEYVAULT_BACKEND_BEARER_TOKEN", "")
	t.Setenv("KEYVAULT_BACKEND_TIMEOUT", "")
	ts := setupServer()
	defer ts.Close()
	base := ts.URL + "/keyvault/test/secrets/retrieve-key"
	resp := doRequest(t, http.MethodPut, base, `{"value":"old-test-key"}`)
	expectStatus(t, resp, http.StatusOK)
	version := resp.Header.Get("X-Miniblue-Secret-Version")
	resp.Body.Close()
	if version == "" {
		t.Fatal("secret version header missing")
	}
	resp = doRequest(t, http.MethodPut, base, `{"value":"new-test-key"}`)
	expectStatus(t, resp, http.StatusOK)
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, base+"/"+version, "")
	expectStatus(t, resp, http.StatusOK)
	if got := decodeJSON(t, resp)["value"]; got != "old-test-key" {
		t.Fatalf("unexpected historical version: %v", got)
	}
	resp.Body.Close()
}

func TestBennyInvalidBackendFailsStartup(t *testing.T) {
	t.Setenv("FUNCTION_BACKEND_URL", "file:///tmp/not-a-backend")
	if _, err := server.NewWithError(); err == nil || !strings.Contains(err.Error(), "FUNCTION_BACKEND_URL") {
		t.Fatalf("expected invalid function backend error, got %v", err)
	}
}
