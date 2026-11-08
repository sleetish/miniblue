// Package functionproxy provides bounded HTTP invocation mocks/passthrough.
// It does not execute or host an Azure Functions runtime.
package functionproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/moabukar/miniblue/internal/azerr"
	"github.com/moabukar/miniblue/internal/store"
)

const maxBodySize = 1 << 20

var functionName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type mockResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type Handler struct {
	base   *url.URL
	apiKey string
	client *http.Client
	mocks  map[string]mockResponse
}

// NewHandler validates all FUNCTION_* settings before serving requests.
func NewHandler(_ *store.Store) (*Handler, error) {
	h := &Handler{mocks: map[string]mockResponse{}, client: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if raw := os.Getenv("FUNCTION_BACKEND_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
			u.RawPath != "" || !safePath(u.Path) {
			return nil, fmt.Errorf("invalid FUNCTION_BACKEND_URL: expected a fixed absolute http(s) URL without userinfo, query, fragment, encoded path, or dot segments")
		}
		h.base = u
	}
	h.apiKey = os.Getenv("FUNCTION_BACKEND_API_KEY")
	if h.apiKey != "" && (h.base == nil || !validHeaderValue(h.apiKey) || strings.TrimSpace(h.apiKey) != h.apiKey) {
		return nil, fmt.Errorf("invalid FUNCTION_BACKEND_API_KEY: requires FUNCTION_BACKEND_URL and a valid header value")
	}
	if raw := os.Getenv("FUNCTION_BACKEND_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 || timeout > time.Minute {
			return nil, fmt.Errorf("invalid FUNCTION_BACKEND_TIMEOUT: expected a duration greater than zero and at most 1m")
		}
		h.client.Timeout = timeout
	}
	if raw := os.Getenv("FUNCTION_MOCK_RESPONSES"); raw != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		start, err := decoder.Token()
		if err != nil || start != json.Delim('{') {
			return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES: expected a JSON object")
		}
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES: expected a string operation key")
			}
			if _, exists := h.mocks[key]; exists {
				return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES: duplicate operation %q", key)
			}
			var mock mockResponse
			if err := decoder.Decode(&mock); err != nil {
				return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES: %w", err)
			}
			h.mocks[key] = mock
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES: incomplete object")
		}
		if err := decoder.Decode(new(interface{})); err != io.EOF {
			return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES: expected one JSON object")
		}
		for key, mock := range h.mocks {
			parts := strings.SplitN(key, " ", 2)
			if len(parts) != 2 || !allowedMethod(parts[0]) || !validMockPath(parts[1]) ||
				mock.Status < 200 || mock.Status > 599 || len(mock.Body) == 0 ||
				len(mock.Body) > maxBodySize || !json.Valid(mock.Body) ||
				((mock.Status == 204 || mock.Status == 205 || mock.Status == 304) && string(mock.Body) != "null") {
				return nil, fmt.Errorf("invalid FUNCTION_MOCK_RESPONSES entry %q: expected METHOD /api/name[/path or /*], status 200..599, and bounded JSON body (null for bodyless statuses)", key)
			}
		}
	}
	return h, nil
}

func validHeaderValue(value string) bool {
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func allowedMethod(method string) bool {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return true
	}
	return false
}

func safePath(path string) bool {
	if strings.ContainsAny(path, "\\%?#") {
		return false
	}
	for _, c := range path {
		if c < 32 || c == 127 {
			return false
		}
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validMockPath(path string) bool {
	wildcard := strings.HasSuffix(path, "/*")
	path = strings.TrimSuffix(path, "/*")
	if !safePath(path) || strings.Contains(path, "*") || !strings.HasPrefix(path, "/api/") {
		return false
	}
	suffix := strings.TrimPrefix(path, "/api/")
	if wildcard && strings.Contains(suffix, "/") {
		return false
	}
	return functionName.MatchString(strings.Split(suffix, "/")[0])
}

// Register adds invocation routes only.
func (h *Handler) Register(r chi.Router) {
	r.HandleFunc("/api/{functionName}", h.Invoke)
	r.HandleFunc("/api/{functionName}/*", h.Invoke)
}

func readBounded(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodySize {
		return nil, fmt.Errorf("body exceeds 1 MiB")
	}
	return data, nil
}

// Invoke forwards body bytes only when a fixed backend is explicitly configured.
// Request headers are allowlisted; query credentials are removed.
func (h *Handler) Invoke(w http.ResponseWriter, r *http.Request) {
	if !allowedMethod(r.Method) {
		azerr.MethodNotAllowed(w, r.Method)
		return
	}
	if !safePath(r.URL.Path) || !validMockPath(r.URL.Path) || strings.Contains(r.URL.Path, "*") {
		azerr.BadRequest(w, "Invalid function route path.")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		azerr.BadRequest(w, "Invalid invocation query.")
		return
	}
	body, err := readBounded(r.Body)
	if err != nil {
		azerr.WriteError(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "Could not read request body within the 1 MiB limit.")
		return
	}
	mock, found := h.mocks[r.Method+" "+r.URL.Path]
	if !found && strings.Count(r.URL.Path, "/") >= 3 {
		name := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")[0]
		mock, found = h.mocks[r.Method+" /api/"+name+"/*"]
	}
	if found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(mock.Status)
		if r.Method != "HEAD" && mock.Status != 204 && mock.Status != 205 && mock.Status != 304 {
			_, _ = w.Write(mock.Body)
		}
		return
	}
	if h.base == nil {
		azerr.WriteError(w, http.StatusNotImplemented, "FunctionInvocationNotConfigured", "No mock response or backend is configured; no function was executed.")
		return
	}
	for key := range query {
		switch strings.ToLower(key) {
		case "code", "key", "api_key", "api-key", "apikey", "x-functions-key", "authorization", "access_token", "token", "sig", "signature", "cookie":
			delete(query, key)
		}
	}
	target := *h.base
	target.Path = strings.TrimRight(h.base.Path, "/") + r.URL.Path
	target.RawPath = ""
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		azerr.WriteError(w, http.StatusBadGateway, "FunctionBackendRequestFailed", "Could not construct backend request.")
		return
	}
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	if h.apiKey != "" {
		req.Header.Set("x-functions-key", h.apiKey)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		status, code := http.StatusBadGateway, "FunctionBackendUnavailable"
		if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
			status, code = http.StatusGatewayTimeout, "FunctionBackendTimeout"
		}
		azerr.WriteError(w, status, code, "Configured function backend request failed.")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		azerr.WriteError(w, http.StatusBadGateway, "FunctionBackendRedirectRejected", "Backend redirects are not followed or relayed.")
		return
	}
	data, err := readBounded(resp.Body)
	if err != nil {
		azerr.WriteError(w, http.StatusBadGateway, "FunctionBackendResponseInvalid", "Could not read backend response within the 1 MiB limit.")
		return
	}
	if value := resp.Header.Get("Content-Type"); value != "" {
		w.Header().Set("Content-Type", value)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != "HEAD" && resp.StatusCode != 204 && resp.StatusCode != 205 {
		_, _ = w.Write(data)
	}
}
