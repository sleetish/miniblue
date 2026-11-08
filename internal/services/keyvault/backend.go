package keyvault

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/moabukar/miniblue/internal/azerr"
	"github.com/moabukar/miniblue/internal/store"
)

const (
	maxRequestBytes  = 1 << 20
	maxResponseBytes = 4 << 20
)

// Config opts into forwarding all secret operations to a single fixed vault.
type Config struct {
	BackendURL         string
	BackendBearerToken string
	BackendTimeout     time.Duration
}

type backend struct {
	url    *url.URL
	token  string
	client *http.Client
}

// NewHandlerFromEnv must be called during startup so configuration errors can
// fail startup. NewHandler intentionally does not read the environment.
func NewHandlerFromEnv(s *store.Store) (*Handler, error) {
	if os.Getenv("KEYVAULT_BACKEND_API_KEY") != "" {
		return nil, fmt.Errorf("KEYVAULT_BACKEND_API_KEY is unsupported; use KEYVAULT_BACKEND_BEARER_TOKEN")
	}
	c := Config{
		BackendURL:         os.Getenv("KEYVAULT_BACKEND_URL"),
		BackendBearerToken: os.Getenv("KEYVAULT_BACKEND_BEARER_TOKEN"),
	}
	if raw := os.Getenv("KEYVAULT_BACKEND_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("KEYVAULT_BACKEND_TIMEOUT must be a positive duration")
		}
		c.BackendTimeout = timeout
	}
	return NewHandlerWithConfig(s, c)
}

func NewHandlerWithConfig(s *store.Store, c Config) (*Handler, error) {
	if c.BackendTimeout < 0 || c.BackendTimeout > time.Minute {
		return nil, fmt.Errorf("Key Vault backend timeout must be between zero and one minute")
	}
	h := NewHandler(s)
	if c.BackendURL == "" {
		if c.BackendBearerToken != "" || c.BackendTimeout != 0 {
			return nil, fmt.Errorf("Key Vault backend settings require KEYVAULT_BACKEND_URL")
		}
		return h, nil
	}
	u, err := url.Parse(c.BackendURL)
	if err != nil || len(c.BackendURL) > 2048 || strings.Contains(c.BackendURL, "#") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, fmt.Errorf("KEYVAULT_BACKEND_URL must be an origin without credentials, path, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return nil, fmt.Errorf("KEYVAULT_BACKEND_URL requires HTTPS (HTTP is allowed only on loopback)")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("KEYVAULT_BACKEND_URL has an invalid port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("KEYVAULT_BACKEND_URL has an empty port")
	}
	if len(c.BackendBearerToken) > 16<<10 || !bearerToken.MatchString(c.BackendBearerToken) {
		return nil, fmt.Errorf("KEYVAULT_BACKEND_BEARER_TOKEN must be a valid nonempty bearer token")
	}
	timeout := c.BackendTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxResponseHeaderBytes = 32 << 10
	h.backend = &backend{url: u, token: c.BackendBearerToken, client: &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	return h, nil
}

var segment = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
var bearerToken = regexp.MustCompile(`^[a-zA-Z0-9._~+/-]+=*$`)

func (h *Handler) forward(w http.ResponseWriter, r *http.Request) bool {
	if h.backend == nil {
		return false
	}
	name, version := chi.URLParam(r, "secretName"), chi.URLParam(r, "version")
	if len(name) > 127 || len(version) > 127 || (name != "" && !segment.MatchString(name)) || (version != "" && !segment.MatchString(version)) || len(r.URL.RawQuery) > 8192 {
		azerr.BadRequest(w, "Invalid backend secret path or query")
		return true
	}
	u := *h.backend.url
	u.Path = "/secrets"
	if name != "" {
		u.Path += "/" + name
		if version != "" {
			u.Path += "/" + version
		} else if strings.HasSuffix(chi.RouteContext(r.Context()).RoutePattern(), "/versions") {
			u.Path += "/versions"
		}
	}
	u.RawQuery = r.URL.RawQuery
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		azerr.WriteError(w, http.StatusRequestEntityTooLarge, "RequestTooLarge", "Backend request exceeds limit")
		return true
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		azerr.BadRequest(w, "Invalid backend request")
		return true
	}
	req.Header.Set("Authorization", "Bearer "+h.backend.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := h.backend.client.Do(req)
	if err != nil {
		azerr.WriteError(w, http.StatusBadGateway, "BackendUnavailable", "Key Vault backend request failed")
		return true
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		azerr.WriteError(w, http.StatusBadGateway, "InvalidBackendResponse", "Key Vault backend response rejected")
		return true
	}
	for _, header := range []string{"Content-Type", "X-Ms-Request-Id", "Retry-After"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(data)
	return true
}
