# Azure AI Search and Azure OpenAI

miniblue provides offline ARM configuration stubs and deterministic data-plane mocks. These APIs are for local application wiring and tests, not Azure service emulation.

## Azure AI Search

`SERVICES=search` enables `Microsoft.Search/searchServices` ARM create/get/list/delete at the standard subscriptions/resource-groups path. It also enables the data plane:

```text
GET/PUT/DELETE /search/{service}/indexes[/{index}]
POST /search/{service}/indexes/{index}/docs/index
POST /search/{service}/indexes/{index}/docs/search
```

Index definitions are stored as supplied and must exist before indexing/searching. Documents use a string `id` key and accept Azure-style `value` actions (`upload`, `merge`, `mergeOrUpload`, and `delete`). Custom index key fields are not implemented. Search uses case-insensitive substring matching on serialized document content, supports `*`/empty search, `top`, and one `field eq 'literal'` filter. Ranking is a deterministic match count, not Azure ranking. `queryType: "semantic"` adds a synthetic `@search.rerankerScore`; it performs no semantic inference and must not be used to calibrate production confidence. Vector queries fail explicitly unless a backend or canned override is configured. Other filter expressions and broader SDK operations are unsupported.

## Azure OpenAI

`SERVICES=openai` enables `Microsoft.CognitiveServices/accounts` ARM CRUD (accounts must have `kind: "OpenAI"`) and nested deployment CRUD. Data-plane routes follow Azure's shape:

```text
POST /openai/deployments/{deployment}/embeddings?api-version=...
POST /openai/deployments/{deployment}/chat/completions?api-version=...
POST /openai/deployments/{deployment}/completions?api-version=...
```

The default embedding response contains deterministic synthetic vectors for `text-embedding-3-small` with 1536 dimensions. `dimensions` may request 1–1536 dimensions, with at most 64 input strings per request. Chat and legacy completion routes return deterministic mock text. No model is loaded, inference is not performed, and token counts are placeholders. Local data-plane deployment names are routing labels; they do not require a previously created ARM deployment or select a real model.

## Explicit data-plane backends and mock overrides

Mocks work offline by default; startup does not make outbound requests. Operators may explicitly configure `SEARCH_BACKEND_URL` or `OPENAI_BACKEND_URL` to a fixed absolute `http://` or `https://` endpoint. Requests and buffered responses are limited to 1 MiB and 10 seconds; oversized/unreadable responses and redirects return 502. Streaming is not supported. Only the operation path and query plus `Content-Type`/`Accept` are relayed. Search removes its local `/search/{service}` routing prefix before forwarding; OpenAI forwards its `/openai/...` route. A configured URL path prefix is preserved. `SEARCH_BACKEND_API_KEY` and `OPENAI_BACKEND_API_KEY` optionally set the backend `api-key`; caller Authorization, API keys, cookies, and other headers are never forwarded. No inbound request can choose a destination. These settings affect only corresponding data-plane routes; ARM APIs always remain local.

For deterministic failures or other canned test responses, configure JSON maps in `SEARCH_MOCK_RESPONSES` and `OPENAI_MOCK_RESPONSES`. Keys are an upper/lower-case-insensitive HTTP method and operation path; Search paths omit `/search/{service}` (for example `POST /indexes/products/docs/search`), while OpenAI paths include `/openai` (for example `POST /openai/deployments/chat/embeddings`). Values are `{"status":429,"body":{"error":{"code":"TooManyRequests"}}}`. Invalid JSON, status codes, or backend URLs fail startup explicitly. A matching override takes precedence over a configured backend.

These are intentionally bounded subsets, not SDK parity: no quota enforcement, API-key authentication, advanced Search query/filter/vector semantics, OpenAI model behavior, streaming, or Azure long-running provisioning is implemented.

## Example: benefits retrieval fixtures

Replace the host below with your Miniblue host:

```bash
BASE=http://localhost:4566
curl --fail-with-body -X PUT "$BASE/search/benny/indexes/benefits" \
  -H 'Content-Type: application/json' \
  -d '{"name":"benefits","fields":[{"name":"id","type":"Edm.String","key":true},{"name":"content","type":"Edm.String","searchable":true},{"name":"planYear","type":"Edm.Int32","filterable":true}]}'
curl --fail-with-body -X POST "$BASE/search/benny/indexes/benefits/docs/index" \
  -H 'Content-Type: application/json' \
  -d '{"value":[{"@search.action":"upload","id":"fixture-1","content":"Dental benefit test fixture","planYear":2026}]}'
curl --fail-with-body -X POST "$BASE/search/benny/indexes/benefits/docs/search" \
  -H 'Content-Type: application/json' \
  -d '{"search":"dental","filter":"planYear eq 2026","queryType":"semantic"}'
curl --fail-with-body -X POST "$BASE/openai/deployments/embedding/embeddings?api-version=2024-02-01" \
  -H 'Content-Type: application/json' -d '{"input":"Dental benefit test fixture"}'
```

For Azure Search clients that can use a path-prefixed endpoint, point them to `http://HOST:4566/search/benny`; Azure deployment-based OpenAI clients use `http://HOST:4566`. Validate your client's endpoint and HTTPS requirements: these examples do not establish full SDK compatibility.

## Portainer/Compose configuration

Add these to the `miniblue` service's `environment` section for an offline 429 fixture, then recreate the container:

```yaml
environment:
  OPENAI_MOCK_RESPONSES: >-
    {"POST /openai/deployments/embedding/embeddings":{"status":429,"body":{"error":{"code":"TooManyRequests","message":"Synthetic rate-limit fixture"}}}}
```

Overrides match the method/path, regardless of request body or query, and are static until restart. Use separate test instances/configurations for success and failure scenarios. Unmatched operations use the default mock or configured backend.

To forward to another mock server instead:

```yaml
environment:
  SEARCH_BACKEND_URL: http://mock-server:8080/search-api
  OPENAI_BACKEND_URL: http://mock-server:8080
```

For real Azure, use the service root URLs, such as `https://YOUR-SERVICE.search.windows.net` and `https://YOUR-ACCOUNT.openai.azure.com`. Supply backend API keys using Portainer environment settings or another appropriate secret mechanism; do not commit keys to Compose or Git. Entra bearer-token acquisition is not implemented.

**Passthrough is a trust boundary:** Miniblue remains unauthenticated. Every client that can reach its AI routes can use your configured backend credentials, incur model charges, index/delete Search content, and send data off-network. Use isolated non-production backends, least-privileged keys and network access controls. Do not expose a credentialed passthrough to untrusted LAN clients. Enabling these backends does not require a Docker socket or real ACI/VM execution.
