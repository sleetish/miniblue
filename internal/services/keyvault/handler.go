package keyvault

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/moabukar/miniblue/internal/azerr"
	"github.com/moabukar/miniblue/internal/store"
)

type Secret struct {
	ID         string            `json:"id"`
	Value      string            `json:"value"`
	Attributes map[string]string `json:"attributes"`
}

type Handler struct {
	store   *store.Store
	mu      sync.Mutex
	backend *backend
}

// NewHandler is always a local mock, independent of process environment.
func NewHandler(s *store.Store) *Handler {
	return &Handler{store: s}
}

func (h *Handler) Register(r chi.Router) {
	r.Route("/keyvault/{vaultName}/secrets", func(r chi.Router) {
		r.Get("/", h.ListSecrets)
		r.Route("/{secretName}", func(r chi.Router) {
			r.Put("/", h.SetSecret)
			r.Get("/", h.GetSecret)
			r.Delete("/", h.DeleteSecret)
			r.Get("/versions", h.ListVersions)
			r.Get("/{version}", h.GetSecret)
		})
	})
}

func (h *Handler) key(vault, name string) string {
	return "kv:" + vault + ":" + name
}

type secretHistory struct {
	Versions []Secret `json:"versions"`
}

// history also accepts JSON-decoded persistent stores and pre-versioning secrets.
func (h *Handler) history(key string) (secretHistory, error) {
	v, ok := h.store.Get(key)
	if !ok {
		return secretHistory{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return secretHistory{}, errors.New("stored secret cannot be encoded")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return secretHistory{}, errors.New("stored secret must be an object")
	}
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return secretHistory{}, errors.New("invalid stored secret key")
	}
	expectedID := "https://" + parts[1] + ".vault.azure.net/secrets/" + parts[2]
	if versions, exists := object["versions"]; exists {
		var entries []json.RawMessage
		if err := json.Unmarshal(versions, &entries); err != nil || len(entries) == 0 {
			return secretHistory{}, errors.New("stored secret history must contain versions")
		}
		history := secretHistory{Versions: make([]Secret, 0, len(entries))}
		seen := make(map[string]bool, len(entries))
		for _, entry := range entries {
			secret, err := decodeStoredSecret(entry)
			if err != nil {
				return secretHistory{}, err
			}
			version, found := strings.CutPrefix(secret.ID, expectedID+"/")
			if !found || version == "" || strings.Contains(version, "/") || seen[version] {
				return secretHistory{}, errors.New("stored secret history has invalid or duplicate version IDs")
			}
			seen[version] = true
			history.Versions = append(history.Versions, secret)
		}
		return history, nil
	}
	legacy, err := decodeStoredSecret(data)
	if err != nil {
		return secretHistory{}, err
	}
	if legacy.ID != expectedID {
		return secretHistory{}, errors.New("stored legacy secret has an invalid ID")
	}
	legacy.ID += "/" + strings.ReplaceAll(uuid.NewString(), "-", "")
	history := secretHistory{Versions: []Secret{legacy}}
	h.store.Set(key, history)
	return history, nil
}

func decodeStoredSecret(data []byte) (Secret, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return Secret{}, errors.New("stored secret version must be an object")
	}
	for _, field := range []string{"id", "value"} {
		var value *string
		if err := json.Unmarshal(fields[field], &value); err != nil || value == nil {
			return Secret{}, errors.New("stored secret requires string ID and value")
		}
	}
	var secret Secret
	if err := json.Unmarshal(data, &secret); err != nil {
		return Secret{}, errors.New("stored secret has invalid fields")
	}
	return secret, nil
}

func storedSecretError(w http.ResponseWriter) {
	azerr.WriteError(w, http.StatusInternalServerError, "CorruptSecretState", "Stored Key Vault secret data is invalid")
}

func unversioned(s Secret) Secret {
	s.ID = s.ID[:strings.LastIndex(s.ID, "/")]
	return s
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func (h *Handler) SetSecret(w http.ResponseWriter, r *http.Request) {
	if h.forward(w, r) {
		return
	}
	vault := chi.URLParam(r, "vaultName")
	name := chi.URLParam(r, "secretName")

	var body struct {
		Value string `json:"value"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := decoder.Decode(&body); err != nil {
		azerr.BadRequest(w, "Invalid or oversized request body")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		azerr.BadRequest(w, "Expected a single JSON object")
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	history, err := h.history(h.key(vault, name))
	if err != nil {
		storedSecretError(w)
		return
	}
	version := strings.ReplaceAll(uuid.NewString(), "-", "")
	secret := Secret{
		ID:    "https://" + vault + ".vault.azure.net/secrets/" + name + "/" + version,
		Value: body.Value,
		Attributes: map[string]string{
			"enabled": "true",
			"created": time.Now().UTC().Format(time.RFC3339),
			"updated": time.Now().UTC().Format(time.RFC3339),
		},
	}

	history.Versions = append(history.Versions, secret)
	h.store.Set(h.key(vault, name), history)
	w.Header().Set("X-Miniblue-Secret-Version", version)
	writeJSON(w, unversioned(secret))
}

func (h *Handler) GetSecret(w http.ResponseWriter, r *http.Request) {
	if h.forward(w, r) {
		return
	}
	vault := chi.URLParam(r, "vaultName")
	name := chi.URLParam(r, "secretName")

	h.mu.Lock()
	defer h.mu.Unlock()
	history, err := h.history(h.key(vault, name))
	if err != nil {
		storedSecretError(w)
		return
	}
	if len(history.Versions) == 0 {
		azerr.NotFound(w, "Microsoft.KeyVault/vaults/secrets", name)
		return
	}
	version := chi.URLParam(r, "version")
	if version == "" {
		writeJSON(w, unversioned(history.Versions[len(history.Versions)-1]))
		return
	}
	for _, secret := range history.Versions {
		if strings.HasSuffix(secret.ID, "/"+version) {
			writeJSON(w, secret)
			return
		}
	}
	azerr.NotFound(w, "Microsoft.KeyVault/vaults/secrets", name+"/"+version)
}

func (h *Handler) DeleteSecret(w http.ResponseWriter, r *http.Request) {
	if h.forward(w, r) {
		return
	}
	vault := chi.URLParam(r, "vaultName")
	name := chi.URLParam(r, "secretName")
	h.mu.Lock()
	defer h.mu.Unlock()
	h.store.Delete(h.key(vault, name))
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) ListSecrets(w http.ResponseWriter, r *http.Request) {
	if h.forward(w, r) {
		return
	}
	vault := chi.URLParam(r, "vaultName")
	h.mu.Lock()
	defer h.mu.Unlock()
	// Store.ListKeysByPrefix falls back to all keys for non-memory backends.
	keys := h.store.ListKeysByPrefix("kv:" + vault + ":")
	// Azure Key Vault list returns metadata only, NOT the secret value
	redacted := make([]map[string]interface{}, 0, len(keys))
	for _, key := range keys {
		if !strings.HasPrefix(key, "kv:"+vault+":") {
			continue
		}
		history, err := h.history(key)
		if err != nil {
			storedSecretError(w)
			return
		}
		if len(history.Versions) > 0 {
			s := unversioned(history.Versions[len(history.Versions)-1])
			redacted = append(redacted, map[string]interface{}{
				"id":         s.ID,
				"attributes": s.Attributes,
			})
		}
	}
	writeJSON(w, map[string]interface{}{"value": redacted})
}

func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	if h.forward(w, r) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	history, err := h.history(h.key(chi.URLParam(r, "vaultName"), chi.URLParam(r, "secretName")))
	if err != nil {
		storedSecretError(w)
		return
	}
	items := make([]map[string]any, 0, len(history.Versions))
	for _, secret := range history.Versions {
		items = append(items, map[string]any{"id": secret.ID, "attributes": secret.Attributes})
	}
	writeJSON(w, map[string]any{"value": items})
}
