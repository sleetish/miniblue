package azureai

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/moabukar/miniblue/internal/azerr"
	"github.com/moabukar/miniblue/internal/store"
)

const maxBodySize = 1 << 20

type mockResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type backend struct {
	base   *url.URL
	apiKey string
	client *http.Client
	mocks  map[string]mockResponse
}

type Handler struct {
	store         *store.Store
	searchBackend backend
	openAIBackend backend
}

func NewHandler(s *store.Store) (*Handler, error) {
	search, err := newBackend("SEARCH")
	if err != nil {
		return nil, err
	}
	openAI, err := newBackend("OPENAI")
	if err != nil {
		return nil, err
	}
	return &Handler{store: s, searchBackend: search, openAIBackend: openAI}, nil
}

func newBackend(prefix string) (backend, error) {
	b := backend{client: &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, mocks: map[string]mockResponse{}}
	if raw := strings.TrimSpace(getenv(prefix + "_BACKEND_URL")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return backend{}, fmt.Errorf("invalid %s_BACKEND_URL: expected an absolute http(s) URL without userinfo, query, or fragment", prefix)
		}
		b.base = u
		b.apiKey = getenv(prefix + "_BACKEND_API_KEY")
	} else if getenv(prefix+"_BACKEND_API_KEY") != "" {
		return backend{}, fmt.Errorf("%s_BACKEND_API_KEY requires %s_BACKEND_URL", prefix, prefix)
	}
	rawMocks := getenv(prefix + "_MOCK_RESPONSES")
	if strings.TrimSpace(rawMocks) != "" {
		if err := json.Unmarshal([]byte(rawMocks), &b.mocks); err != nil {
			return backend{}, fmt.Errorf("invalid %s_MOCK_RESPONSES JSON: %w", prefix, err)
		}
		normalized := make(map[string]mockResponse, len(b.mocks))
		for key, mock := range b.mocks {
			if mock.Status < 200 || mock.Status > 599 || len(mock.Body) == 0 || !json.Valid(mock.Body) {
				return backend{}, fmt.Errorf("invalid %s_MOCK_RESPONSES entry %q: status must be 200..599 and body valid JSON", prefix, key)
			}
			normalizedKey := strings.ToUpper(strings.TrimSpace(key))
			if _, exists := normalized[normalizedKey]; exists {
				return backend{}, fmt.Errorf("duplicate %s_MOCK_RESPONSES operation %q", prefix, key)
			}
			normalized[normalizedKey] = mock
		}
		b.mocks = normalized
	}
	return b, nil
}

func getenv(key string) string {
	return os.Getenv(key)
}

func (h *Handler) Register(r chi.Router) {
	h.RegisterSearch(r)
	h.RegisterOpenAI(r)
}

func (h *Handler) RegisterSearch(r chi.Router) {
	r.Route("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Search/searchServices", func(r chi.Router) {
		r.Get("/", h.listSearchServices)
		r.Route("/{serviceName}", func(r chi.Router) {
			r.Put("/", h.putSearchService)
			r.Get("/", h.getSearchService)
			r.Delete("/", h.deleteSearchService)
		})
	})
	r.Route("/search/{serviceName}/indexes", func(r chi.Router) {
		r.Get("/", h.listIndexes)
		r.Route("/{indexName}", func(r chi.Router) {
			r.Put("/", h.putIndex)
			r.Get("/", h.getIndex)
			r.Delete("/", h.deleteIndex)
			r.Post("/docs/index", h.indexDocuments)
			r.Post("/docs/search", h.searchDocuments)
		})
	})
}

func (h *Handler) RegisterOpenAI(r chi.Router) {
	r.Route("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.CognitiveServices/accounts", func(r chi.Router) {
		r.Get("/", h.listAccounts)
		r.Route("/{accountName}", func(r chi.Router) {
			r.Put("/", h.putAccount)
			r.Get("/", h.getAccount)
			r.Delete("/", h.deleteAccount)
			r.Route("/deployments", func(r chi.Router) {
				r.Get("/", h.listDeployments)
				r.Route("/{deploymentName}", func(r chi.Router) {
					r.Put("/", h.putDeployment)
					r.Get("/", h.getDeployment)
					r.Delete("/", h.deleteDeployment)
				})
			})
		})
	})

	r.Route("/openai/deployments/{deploymentName}", func(r chi.Router) {
		r.Post("/embeddings", h.embeddings)
		r.Post("/chat/completions", h.chatCompletions)
		r.Post("/completions", h.chatCompletions)
	})
}

func (h *Handler) armKey(prefix, sub, rg, name string) string {
	return prefix + ":" + sub + ":" + rg + ":" + name
}

func decodeBody(r *http.Request, out interface{}) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		return err
	}
	if len(data) > maxBodySize {
		return fmt.Errorf("request body exceeds 1 MiB")
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (h *Handler) armResponse(w http.ResponseWriter, r *http.Request, prefix, typ, name string) {
	key := h.armKey(prefix, chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), name)
	v, ok := h.store.Get(key)
	if !ok {
		azerr.NotFound(w, typ, name)
		return
	}
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) listARM(w http.ResponseWriter, prefix, sub, rg string) {
	items := h.store.ListByPrefix(prefix + ":" + sub + ":" + rg + ":")
	json.NewEncoder(w).Encode(map[string]interface{}{"value": items})
}

func (h *Handler) writeARM(w http.ResponseWriter, r *http.Request, prefix, typ, name string, body map[string]interface{}, props map[string]interface{}) {
	sub, rg := chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName")
	key := h.armKey(prefix, sub, rg, name)
	_, exists := h.store.Get(key)
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s", sub, rg, typ, name)
	resource := map[string]interface{}{"id": id, "name": name, "type": typ, "location": "eastus", "properties": props}
	for k, v := range body {
		if k != "properties" && k != "name" && k != "id" && k != "type" {
			resource[k] = v
		}
	}
	if v, ok := body["properties"].(map[string]interface{}); ok {
		for k, val := range v {
			props[k] = val
		}
	}
	h.store.Set(key, resource)
	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	json.NewEncoder(w).Encode(resource)
}

func (h *Handler) deleteARM(w http.ResponseWriter, r *http.Request, prefix, typ, name string) {
	key := h.armKey(prefix, chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), name)
	if !h.store.Delete(key) {
		azerr.NotFound(w, typ, name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listSearchServices(w http.ResponseWriter, r *http.Request) {
	h.listARM(w, "search:service", chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"))
}
func (h *Handler) putSearchService(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	name := chi.URLParam(r, "serviceName")
	props := map[string]interface{}{"provisioningState": "Succeeded", "status": "running", "hostingMode": "default", "replicaCount": 1, "partitionCount": 1}
	h.writeARM(w, r, "search:service", "Microsoft.Search/searchServices", name, body, props)
}
func (h *Handler) getSearchService(w http.ResponseWriter, r *http.Request) {
	h.armResponse(w, r, "search:service", "Microsoft.Search/searchServices", chi.URLParam(r, "serviceName"))
}
func (h *Handler) deleteSearchService(w http.ResponseWriter, r *http.Request) {
	h.deleteARM(w, r, "search:service", "Microsoft.Search/searchServices", chi.URLParam(r, "serviceName"))
}
func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	h.listARM(w, "openai:account", chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"))
}
func (h *Handler) putAccount(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	name := chi.URLParam(r, "accountName")
	props := map[string]interface{}{"provisioningState": "Succeeded", "endpoint": "https://" + name + ".openai.azure.com/", "disableLocalAuth": false}
	kind := body["kind"]
	if kind != "OpenAI" {
		azerr.BadRequest(w, `Account kind must be "OpenAI".`)
		return
	}
	h.writeARM(w, r, "openai:account", "Microsoft.CognitiveServices/accounts", name, body, props)
}
func (h *Handler) getAccount(w http.ResponseWriter, r *http.Request) {
	h.armResponse(w, r, "openai:account", "Microsoft.CognitiveServices/accounts", chi.URLParam(r, "accountName"))
}
func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "accountName")
	h.store.DeleteByPrefix(h.armKey("openai:deployment", chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), name) + ":")
	h.deleteARM(w, r, "openai:account", "Microsoft.CognitiveServices/accounts", name)
}
func (h *Handler) deploymentPrefix(r *http.Request) string {
	return h.armKey("openai:deployment", chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), chi.URLParam(r, "accountName")) + ":"
}
func (h *Handler) listDeployments(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{"value": h.store.ListByPrefix(h.deploymentPrefix(r))})
}
func (h *Handler) putDeployment(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	name := chi.URLParam(r, "deploymentName")
	if !h.store.Exists(h.armKey("openai:account", chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), chi.URLParam(r, "accountName"))) {
		azerr.NotFound(w, "Microsoft.CognitiveServices/accounts", chi.URLParam(r, "accountName"))
		return
	}
	key := h.deploymentPrefix(r) + name
	_, exists := h.store.Get(key)
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.CognitiveServices/accounts/%s/deployments/%s",
		chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), chi.URLParam(r, "accountName"), name)
	res := map[string]interface{}{"id": id, "name": name, "type": "Microsoft.CognitiveServices/accounts/deployments", "properties": map[string]interface{}{"provisioningState": "Succeeded", "model": map[string]interface{}{"format": "OpenAI", "name": "text-embedding-3-small", "version": "1"}}}
	if p, ok := body["properties"].(map[string]interface{}); ok {
		res["properties"] = p
		p["provisioningState"] = "Succeeded"
	}
	if v, ok := body["sku"]; ok {
		res["sku"] = v
	}
	h.store.Set(key, res)
	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	json.NewEncoder(w).Encode(res)
}
func (h *Handler) getDeployment(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "deploymentName")
	if v, ok := h.store.Get(h.deploymentPrefix(r) + name); ok {
		json.NewEncoder(w).Encode(v)
		return
	}
	azerr.NotFound(w, "Microsoft.CognitiveServices/accounts/deployments", name)
}
func (h *Handler) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "deploymentName")
	if !h.store.Delete(h.deploymentPrefix(r) + name) {
		azerr.NotFound(w, "Microsoft.CognitiveServices/accounts/deployments", name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) dataPlane(w http.ResponseWriter, r *http.Request, b backend, operationPath string) bool {
	key := strings.ToUpper(r.Method + " " + operationPath)
	mock, hasMock := b.mocks[key]
	if !hasMock && b.base == nil {
		return false
	}
	if r.ContentLength > maxBodySize {
		azerr.WriteError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge", "Request body exceeds 1 MiB.")
		return true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		azerr.WriteError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge", "Request body exceeds 1 MiB.")
		return true
	}
	if hasMock {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(mock.Status)
		w.Write(mock.Body)
		return true
	}
	target := *b.base
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(operationPath, "/")
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		azerr.WriteError(w, 500, "BackendError", "Unable to construct backend request.")
		return true
	}
	for _, header := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}
	if b.apiKey != "" {
		req.Header.Set("api-key", b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		if urlErr, ok := err.(*url.Error); ok {
			err = urlErr.Err
		}
		log.Printf("Azure AI backend request failed (%s %s): %v", r.Method, r.URL.Path, err)
		azerr.WriteError(w, http.StatusBadGateway, "BackendError", "Configured Azure AI backend request failed.")
		return true
	}
	defer resp.Body.Close()
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		azerr.WriteError(w, http.StatusBadGateway, "BackendStreamingUnsupported", "Streaming backend responses are not supported.")
		return true
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		log.Printf("Azure AI backend redirect rejected (%s %s)", r.Method, r.URL.Path)
		azerr.WriteError(w, http.StatusBadGateway, "BackendRedirect", "Configured backend redirects are not supported.")
		return true
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil || len(responseBody) > maxBodySize {
		log.Printf("Azure AI backend response unreadable or exceeds 1 MiB (%s %s)", r.Method, r.URL.Path)
		azerr.WriteError(w, http.StatusBadGateway, "BackendResponseError", "Configured backend response could not be read within the 1 MiB limit.")
		return true
	}
	if resp.StatusCode >= http.StatusBadRequest {
		log.Printf("Azure AI backend returned status %d for %s %s", resp.StatusCode, r.Method, r.URL.Path)
	}
	for _, header := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(responseBody)
	return true
}

func (h *Handler) searchPrefix(service, index string) string {
	return "search:doc:" + service + ":" + index + ":"
}
func (h *Handler) indexKey(service, index string) string {
	return "search:index:" + service + ":" + index
}
func (h *Handler) listIndexes(w http.ResponseWriter, r *http.Request) {
	if h.dataPlane(w, r, h.searchBackend, strings.TrimPrefix(r.URL.Path, "/search/"+chi.URLParam(r, "serviceName"))) {
		return
	}
	items := h.store.ListByPrefix("search:index:" + chi.URLParam(r, "serviceName") + ":")
	json.NewEncoder(w).Encode(map[string]interface{}{"value": items})
}
func (h *Handler) putIndex(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/search/"+chi.URLParam(r, "serviceName"))
	if h.dataPlane(w, r, h.searchBackend, op) {
		return
	}
	var index map[string]interface{}
	if err := decodeBody(r, &index); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	if index == nil {
		index = map[string]interface{}{}
	}
	name := chi.URLParam(r, "indexName")
	_, exists := h.store.Get(h.indexKey(chi.URLParam(r, "serviceName"), name))
	if index["name"] == nil {
		index["name"] = name
	}
	if index["name"] != name {
		azerr.BadRequest(w, "Index name must match the request path.")
		return
	}
	h.store.Set(h.indexKey(chi.URLParam(r, "serviceName"), name), index)
	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
	json.NewEncoder(w).Encode(index)
}
func (h *Handler) getIndex(w http.ResponseWriter, r *http.Request) {
	if h.dataPlane(w, r, h.searchBackend, strings.TrimPrefix(r.URL.Path, "/search/"+chi.URLParam(r, "serviceName"))) {
		return
	}
	name := chi.URLParam(r, "indexName")
	if v, ok := h.store.Get(h.indexKey(chi.URLParam(r, "serviceName"), name)); ok {
		json.NewEncoder(w).Encode(v)
		return
	}
	azerr.NotFound(w, "Search/indexes", name)
}
func (h *Handler) deleteIndex(w http.ResponseWriter, r *http.Request) {
	if h.dataPlane(w, r, h.searchBackend, strings.TrimPrefix(r.URL.Path, "/search/"+chi.URLParam(r, "serviceName"))) {
		return
	}
	name := chi.URLParam(r, "indexName")
	h.store.DeleteByPrefix(h.searchPrefix(chi.URLParam(r, "serviceName"), name))
	if !h.store.Delete(h.indexKey(chi.URLParam(r, "serviceName"), name)) {
		azerr.NotFound(w, "Search/indexes", name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) indexDocuments(w http.ResponseWriter, r *http.Request) {
	service, index := chi.URLParam(r, "serviceName"), chi.URLParam(r, "indexName")
	op := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/docs/index"), "/search/"+service)
	if h.dataPlane(w, r, h.searchBackend, op+"/docs/index") {
		return
	}
	if !h.store.Exists(h.indexKey(service, index)) {
		azerr.NotFound(w, "Search/indexes", index)
		return
	}
	var payload struct {
		Value []map[string]interface{} `json:"value"`
	}
	if err := decodeBody(r, &payload); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	results := make([]map[string]interface{}, 0, len(payload.Value))
	for _, doc := range payload.Value {
		id, _ := doc["id"].(string)
		action, _ := doc["@search.action"].(string)
		result := map[string]interface{}{"key": id, "status": true, "statusCode": 201}
		if id == "" {
			result["status"] = false
			result["statusCode"] = http.StatusBadRequest
			result["errorMessage"] = "Document id is required."
			results = append(results, result)
			continue
		}
		switch strings.ToLower(action) {
		case "upload":
			delete(doc, "@search.action")
			h.store.Set(h.searchPrefix(service, index)+id, doc)
		case "merge", "mergeorupload":
			if old, ok := h.store.Get(h.searchPrefix(service, index) + id); !ok {
				if strings.EqualFold(action, "mergeorupload") {
					delete(doc, "@search.action")
					h.store.Set(h.searchPrefix(service, index)+id, doc)
				} else {
					result["status"] = false
					result["statusCode"] = 404
					result["errorMessage"] = "Document not found."
				}
			} else if previous, ok := old.(map[string]interface{}); ok {
				merged := make(map[string]interface{}, len(previous)+len(doc))
				for k, v := range previous {
					merged[k] = v
				}
				for k, v := range doc {
					if k != "@search.action" {
						merged[k] = v
					}
				}
				h.store.Set(h.searchPrefix(service, index)+id, merged)
			}
		case "delete":
			if !h.store.Delete(h.searchPrefix(service, index) + id) {
				result["status"] = false
				result["statusCode"] = 404
				result["errorMessage"] = "Document not found."
			}
		default:
			result["status"] = false
			result["statusCode"] = 400
			result["errorMessage"] = "Unsupported @search.action."
		}
		results = append(results, result)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"value": results})
}

var filterExpr = regexp.MustCompile(`^\s*([A-Za-z0-9_@.-]+)\s+eq\s+(.+?)\s*$`)
var filterLiteral = regexp.MustCompile(`^(?:'[^']*'|"[^"]*"|-?[0-9]+(?:\.[0-9]+)?|true|false)$`)

func matchesFilter(doc map[string]interface{}, filter string) bool {
	if filter == "" {
		return true
	}
	m := filterExpr.FindStringSubmatch(filter)
	if m == nil {
		return false
	}
	value := strings.Trim(m[2], "'\"")
	actual, ok := doc[m[1]]
	if !ok {
		return false
	}
	raw := strings.TrimSpace(m[2])
	if number, ok := actual.(float64); ok && !strings.HasPrefix(raw, "'") && !strings.HasPrefix(raw, `"`) {
		expected, err := strconv.ParseFloat(raw, 64)
		return err == nil && expected == number
	}
	if actualBool, ok := actual.(bool); ok && !strings.HasPrefix(raw, "'") && !strings.HasPrefix(raw, `"`) {
		expected, err := strconv.ParseBool(raw)
		return err == nil && expected == actualBool
	}
	return strings.EqualFold(fmt.Sprint(actual), value)
}
func (h *Handler) searchDocuments(w http.ResponseWriter, r *http.Request) {
	service, index := chi.URLParam(r, "serviceName"), chi.URLParam(r, "indexName")
	op := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/search/"+service), "/docs/search")
	if h.dataPlane(w, r, h.searchBackend, op+"/docs/search") {
		return
	}
	if !h.store.Exists(h.indexKey(service, index)) {
		azerr.NotFound(w, "Search/indexes", index)
		return
	}
	var req struct {
		Search        string          `json:"search"`
		Filter        string          `json:"filter"`
		Top           int             `json:"top"`
		QueryType     string          `json:"queryType"`
		VectorQueries json.RawMessage `json:"vectorQueries"`
	}
	if err := decodeBody(r, &req); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	if len(req.VectorQueries) > 0 && string(req.VectorQueries) != "null" {
		azerr.BadRequest(w, "Vector queries require a configured backend or mock response override.")
		return
	}
	if req.Filter != "" {
		filter := filterExpr.FindStringSubmatch(req.Filter)
		if filter == nil || !filterLiteral.MatchString(filter[2]) {
			azerr.BadRequest(w, "Only a single field eq string, number, or boolean filter is supported.")
			return
		}
	}
	if req.Top <= 0 || req.Top > 1000 {
		req.Top = 50
	}
	words := strings.Fields(strings.ToLower(req.Search))
	type scored struct {
		doc   map[string]interface{}
		score float64
	}
	matches := []scored{}
	for _, item := range h.store.ListByPrefix(h.searchPrefix(service, index)) {
		doc, ok := item.(map[string]interface{})
		if !ok || !matchesFilter(doc, req.Filter) {
			continue
		}
		text, _ := json.Marshal(doc)
		lower := strings.ToLower(string(text))
		score := 0.0
		if req.Search == "" || req.Search == "*" {
			score = 1
		} else {
			for _, word := range words {
				if strings.Contains(lower, word) {
					score++
				}
			}
		}
		if score > 0 {
			matches = append(matches, scored{doc, score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			return fmt.Sprint(matches[i].doc["id"]) < fmt.Sprint(matches[j].doc["id"])
		}
		return matches[i].score > matches[j].score
	})
	totalMatches := len(matches)
	if len(matches) > req.Top {
		matches = matches[:req.Top]
	}
	value := make([]map[string]interface{}, 0, len(matches))
	for _, match := range matches {
		doc := make(map[string]interface{}, len(match.doc)+2)
		for k, v := range match.doc {
			doc[k] = v
		}
		doc["@search.score"] = match.score
		if strings.EqualFold(req.QueryType, "semantic") {
			doc["@search.rerankerScore"] = min(4.0, 1+match.score/10)
		}
		value = append(value, doc)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"value": value, "@odata.count": totalMatches})
}

func (h *Handler) openAIDataPlane(w http.ResponseWriter, r *http.Request) bool {
	return h.dataPlane(w, r, h.openAIBackend, r.URL.Path)
}
func (h *Handler) embeddings(w http.ResponseWriter, r *http.Request) {
	if h.openAIDataPlane(w, r) {
		return
	}
	var req struct {
		Input      interface{} `json:"input"`
		Model      string      `json:"model"`
		Dimensions int         `json:"dimensions"`
	}
	if err := decodeBody(r, &req); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	dimensions := req.Dimensions
	if dimensions == 0 {
		dimensions = 1536
	}
	if dimensions < 1 || dimensions > 1536 {
		azerr.BadRequest(w, "Synthetic embeddings support dimensions from 1 through 1536.")
		return
	}
	inputs := []string{}
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []interface{}:
		if len(v) > 64 {
			azerr.BadRequest(w, "Synthetic embeddings support at most 64 input strings per request.")
			return
		}
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				azerr.BadRequest(w, "Embedding input items must be strings.")
				return
			}
			inputs = append(inputs, s)
		}
	default:
		azerr.BadRequest(w, "Embedding input must be a string or an array of strings.")
		return
	}
	if len(inputs) == 0 {
		azerr.BadRequest(w, "Embedding input must not be empty.")
		return
	}
	data := make([]map[string]interface{}, 0, len(inputs))
	for i, text := range inputs {
		sum := sha256.Sum256([]byte(text))
		vector := make([]float64, dimensions)
		for d := range vector {
			chunk := sha256.Sum256(append(sum[:], byte(d>>8), byte(d)))
			n := binary.BigEndian.Uint32(chunk[:4])
			vector[d] = (float64(n)/float64(^uint32(0)))*2 - 1
		}
		data = append(data, map[string]interface{}{"object": "embedding", "index": i, "embedding": vector})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data, "model": "text-embedding-3-small", "usage": map[string]int{"prompt_tokens": 0, "total_tokens": 0}})
}
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if h.openAIDataPlane(w, r) {
		return
	}
	var req struct {
		Messages []map[string]interface{} `json:"messages"`
		Prompt   interface{}              `json:"prompt"`
		Model    string                   `json:"model"`
		Stream   bool                     `json:"stream"`
	}
	if err := decodeBody(r, &req); err != nil {
		azerr.BadRequest(w, "Invalid JSON request body.")
		return
	}
	if req.Stream {
		azerr.BadRequest(w, "Streaming is not supported.")
		return
	}
	if len(req.Messages) == 0 && req.Prompt == nil {
		azerr.BadRequest(w, "Messages or a prompt are required.")
		return
	}
	content := "This is a deterministic miniblue mock response; no model inference was performed."
	if len(req.Messages) > 0 {
		if v, ok := req.Messages[len(req.Messages)-1]["content"].(string); ok && v != "" {
			content = "Mock response to: " + v
		}
	}
	resp := map[string]interface{}{"id": "chatcmpl-miniblue", "object": "chat.completion", "created": 0, "model": chi.URLParam(r, "deploymentName"), "choices": []map[string]interface{}{{"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
	if req.Prompt != nil {
		resp["object"] = "text_completion"
		resp["choices"] = []map[string]interface{}{{"index": 0, "text": content, "finish_reason": "stop"}}
	}
	json.NewEncoder(w).Encode(resp)
}
