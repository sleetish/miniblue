// Package monitor implements configuration and a small, non-Azure-compatible
// telemetry record API for local testing.
package monitor

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/moabukar/miniblue/internal/azerr"
	"github.com/moabukar/miniblue/internal/store"
)

const maxBody = 1 << 20
const maxRecordBody = 16 << 10
const maxRecords = 1000

var resourceTypes = [...]string{
	"Microsoft.Insights/components",
	"Microsoft.OperationalInsights/workspaces",
	"Microsoft.Insights/actionGroups",
	"Microsoft.Insights/metricAlerts",
	"Microsoft.Insights/workbooks",
}

type Handler struct {
	store   store.Backend
	backend *url.URL
	apiKey  string
	client  *http.Client
	mu      sync.Mutex
}

// NewHandler validates operator-controlled backend settings at startup.
func NewHandler(s store.Backend) (*Handler, error) {
	if s == nil {
		return nil, fmt.Errorf("monitor: storage backend is required")
	}
	h := &Handler{store: s, client: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	raw := strings.TrimSpace(os.Getenv("MONITOR_BACKEND_URL"))
	h.apiKey = os.Getenv("MONITOR_BACKEND_API_KEY")
	if raw == "" {
		if h.apiKey != "" {
			return nil, fmt.Errorf("MONITOR_BACKEND_API_KEY requires MONITOR_BACKEND_URL")
		}
		return h, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("invalid MONITOR_BACKEND_URL: expected an absolute http(s) URL without credentials, query, or fragment")
	}
	h.backend = u
	return h, nil
}

func (h *Handler) Register(r chi.Router) {
	for _, typ := range resourceTypes {
		typ := typ
		base := "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/" + typ
		r.Get(base, func(w http.ResponseWriter, r *http.Request) { h.listResources(w, r, typ) })
		r.Get(base+"/", func(w http.ResponseWriter, r *http.Request) { h.listResources(w, r, typ) })
		r.Put(base+"/{name}", func(w http.ResponseWriter, r *http.Request) { h.putResource(w, r, typ) })
		r.Get(base+"/{name}", func(w http.ResponseWriter, r *http.Request) { h.getResource(w, r, typ) })
		r.Delete(base+"/{name}", func(w http.ResponseWriter, r *http.Request) { h.deleteResource(w, r, typ) })
	}
	r.Post("/monitor/records", h.postRecord)
	r.Get("/monitor/records", h.listRecords)
	r.Delete("/monitor/records", h.deleteRecords)
}

func segment(s string) string { return hex.EncodeToString([]byte(strings.ToLower(s))) }

func armPrefix(typ, sub, rg string) string {
	return "monitor:arm:" + segment(typ) + ":" + segment(sub) + ":" + segment(rg) + ":"
}

func resourcePrefix(r *http.Request, typ string) string {
	return armPrefix(typ, chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"))
}

// CleanupResourcesInRG removes only monitor ARM configuration for the resource
// group. Telemetry records are not resource-group scoped and are not deleted.
// The backend interface does not expose errors from DeleteByPrefix.
func CleanupResourcesInRG(s interface{ DeleteByPrefix(string) int }, sub, rg string) {
	for _, typ := range resourceTypes {
		s.DeleteByPrefix(armPrefix(typ, sub, rg))
	}
}

func resourceKey(r *http.Request, typ string) string {
	return resourcePrefix(r, typ) + segment(chi.URLParam(r, "name"))
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("could not read request body")
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("request body exceeds 1 MiB")
	}
	return b, nil
}

func object(b []byte) (map[string]json.RawMessage, error) {
	var v map[string]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil || v == nil {
		return nil, fmt.Errorf("body must be a JSON object")
	}
	return v, nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Once headers are sent, the HTTP server can only terminate the response.
		panic(http.ErrAbortHandler)
	}
}

func (h *Handler) putResource(w http.ResponseWriter, r *http.Request, typ string) {
	b, err := readBody(r)
	if err != nil {
		azerr.BadRequest(w, err.Error())
		return
	}
	resource, err := object(b)
	if err != nil {
		azerr.BadRequest(w, err.Error())
		return
	}
	if p, ok := resource["properties"]; ok {
		if _, err := object(p); err != nil {
			azerr.BadRequest(w, "properties must be a JSON object")
			return
		}
	}
	name := chi.URLParam(r, "name")
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s", chi.URLParam(r, "subscriptionId"), chi.URLParam(r, "resourceGroupName"), typ, name)
	for k, v := range map[string]string{"id": id, "name": name, "type": typ} {
		resource[k], _ = json.Marshal(v)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, exists := h.store.Get(resourceKey(r, typ))
	if err := h.store.Set(resourceKey(r, typ), resource); err != nil {
		azerr.WriteError(w, 500, "StorageError", "Unable to save resource.")
		return
	}
	status := http.StatusCreated
	if exists {
		status = http.StatusOK
	}
	writeJSON(w, status, resource)
}

func (h *Handler) getResource(w http.ResponseWriter, r *http.Request, typ string) {
	v, ok := h.store.Get(resourceKey(r, typ))
	if !ok {
		azerr.NotFound(w, typ, chi.URLParam(r, "name"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) listResources(w http.ResponseWriter, r *http.Request, typ string) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"value": h.store.ListByPrefix(resourcePrefix(r, typ))})
}

func (h *Handler) deleteResource(w http.ResponseWriter, r *http.Request, typ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.store.Delete(resourceKey(r, typ)) {
		azerr.NotFound(w, typ, chi.URLParam(r, "name"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type record struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	Timestamp      string          `json:"timestamp"`
	Type           string          `json:"type"`
	Data           json.RawMessage `json:"data"`
}

func recordPrefix(conversation string) string {
	return "monitor:record:" + hex.EncodeToString([]byte(conversation)) + ":"
}

func validateRecord(b []byte) (record, error) {
	var v record
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("invalid record JSON")
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return v, fmt.Errorf("invalid record JSON")
	}
	if v.ID != "" || strings.TrimSpace(v.ConversationID) == "" || len(v.ConversationID) > 256 {
		return v, fmt.Errorf("conversation_id must be nonempty and at most 256 bytes; id is server-generated")
	}
	if _, err := time.Parse(time.RFC3339Nano, v.Timestamp); err != nil {
		return v, fmt.Errorf("timestamp must be RFC3339")
	}
	if v.Type != "telemetry" && v.Type != "outcome" {
		return v, fmt.Errorf("type must be telemetry or outcome")
	}
	if _, err := object(v.Data); err != nil {
		return v, fmt.Errorf("data must be a JSON object")
	}
	return v, nil
}

func (h *Handler) postRecord(w http.ResponseWriter, r *http.Request) {
	b, err := readBody(r)
	if err != nil {
		azerr.BadRequest(w, err.Error())
		return
	}
	v, err := validateRecord(b)
	if err != nil {
		azerr.BadRequest(w, err.Error())
		return
	}
	if len(b) > maxRecordBody {
		azerr.WriteError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge", "Record exceeds 16 KiB.")
		return
	}
	if r.URL.RawQuery != "" {
		azerr.BadRequest(w, "query parameters are not supported")
		return
	}
	if h.backend != nil {
		h.forward(w, r, b, nil)
		return
	}
	v.ID = uuid.NewString()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.store.CountByPrefix(recordPrefix(v.ConversationID)) >= maxRecords {
		azerr.WriteError(w, http.StatusConflict, "RecordLimitExceeded", "Conversation record limit reached.")
		return
	}
	if err := h.store.Set(recordPrefix(v.ConversationID)+v.ID, v); err != nil {
		azerr.WriteError(w, 500, "StorageError", "Unable to save record.")
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func recordQuery(r *http.Request, deletion bool) (string, *time.Time, url.Values, error) {
	b, err := readBody(r)
	if err != nil {
		return "", nil, nil, err
	}
	if len(b) != 0 {
		return "", nil, nil, fmt.Errorf("request body is not supported")
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", nil, nil, fmt.Errorf("invalid query string")
	}
	for k, v := range q {
		if len(v) != 1 || (k != "conversation_id" && (!deletion || k != "before")) {
			return "", nil, nil, fmt.Errorf("unsupported or repeated query parameter")
		}
	}
	conversation := q.Get("conversation_id")
	if strings.TrimSpace(conversation) == "" || len(conversation) > 256 {
		return "", nil, nil, fmt.Errorf("conversation_id is required and must be at most 256 bytes")
	}
	var before *time.Time
	if _, ok := q["before"]; ok {
		t, err := time.Parse(time.RFC3339Nano, q.Get("before"))
		if err != nil {
			return "", nil, nil, fmt.Errorf("before must be RFC3339")
		}
		before = &t
	}
	return conversation, before, q, nil
}

func (h *Handler) listRecords(w http.ResponseWriter, r *http.Request) {
	conversation, _, q, err := recordQuery(r, false)
	if err != nil {
		azerr.BadRequest(w, err.Error())
		return
	}
	if h.backend != nil {
		h.forward(w, r, nil, q)
		return
	}
	items := h.store.ListByPrefix(recordPrefix(conversation))
	b, err := json.Marshal(map[string]interface{}{"value": items})
	if err != nil {
		azerr.WriteError(w, 500, "StorageError", "Unable to encode stored records.")
		return
	}
	if len(b) > maxBody {
		azerr.WriteError(w, http.StatusRequestEntityTooLarge, "ResponseTooLarge", "Conversation records exceed the response limit.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(b); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (h *Handler) deleteRecords(w http.ResponseWriter, r *http.Request) {
	conversation, before, q, err := recordQuery(r, true)
	if err != nil {
		azerr.BadRequest(w, err.Error())
		return
	}
	if h.backend != nil {
		h.forward(w, r, nil, q)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := recordPrefix(conversation)
	for _, v := range h.store.ListByPrefix(prefix) {
		b, err := json.Marshal(v)
		if err != nil {
			azerr.WriteError(w, 500, "StorageError", "Unable to read stored record.")
			return
		}
		var rec record
		if err := json.Unmarshal(b, &rec); err != nil {
			azerr.WriteError(w, 500, "StorageError", "Unable to read stored record.")
			return
		}
		if _, err := uuid.Parse(rec.ID); err != nil || rec.ConversationID != conversation {
			azerr.WriteError(w, 500, "StorageError", "Invalid stored record.")
			return
		}
		t, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err != nil {
			azerr.WriteError(w, 500, "StorageError", "Invalid stored record timestamp.")
			return
		}
		if (before == nil || t.Before(*before)) && !h.store.Delete(prefix+rec.ID) {
			azerr.WriteError(w, 500, "StorageError", "Unable to delete record.")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte, query url.Values) {
	target := *h.backend
	target.Path = strings.TrimRight(target.Path, "/") + "/monitor/records"
	target.RawPath = ""
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		azerr.WriteError(w, 502, "BackendError", "Unable to create backend request.")
		return
	}
	req.Header.Set("Accept", "application/json")
	if r.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.apiKey != "" {
		req.Header.Set("api-key", h.apiKey)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		azerr.WriteError(w, 502, "BackendError", "Monitor backend request failed.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		azerr.WriteError(w, 502, "BackendError", "Monitor backend redirects are not allowed.")
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(b) > maxBody {
		azerr.WriteError(w, 502, "BackendError", "Monitor backend response could not be read or exceeds 1 MiB.")
		return
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(b); err != nil {
		panic(http.ErrAbortHandler)
	}
}
