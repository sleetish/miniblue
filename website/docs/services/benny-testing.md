# Functions and monitoring test stubs

These features provide controlled test doubles, not an Azure hosting or
observability environment. They work offline by default. Configuration is read
at startup; invalid backend settings fail startup. Use only synthetic data.

## Functions HTTP invocation

The existing `Microsoft.Web/sites` ARM endpoints store function app and
configuration metadata, but do not deploy code, execute functions, resolve Key
Vault references, enforce function keys, or provide warm instances.
`SERVICES=sites` enables both these endpoints and `/api/{functionName}` plus
descendant paths such as `/api/benny/start`.

Configure deterministic responses in Portainer's service environment:

```yaml
environment:
  FUNCTION_MOCK_RESPONSES: >-
    {"POST /api/retrieve":{"status":500,"body":{"error":"synthetic failure"}},
     "POST /api/benny/*":{"status":200,"body":{"status":"fixture"}}}
```

Keys are case-sensitive uppercase method and path; `/*` matches descendants of
one function, not the function root itself. Exact matches take precedence.
Bodies must be JSON; `null` is required for 204, 205 and 304 responses.
Unmatched calls return 501 unless a backend is configured. These responses do
not run Benny's guards, scrubbing or business logic. Run actual code separately.
OPTIONS is handled by the global CORS middleware, not the invocation handler.

### Portainer Compose example

Use `docker-compose.benny.yml` as the Compose path when deploying this
repository's `main` branch as a Git stack. It publishes ports 4566 and 4567
on all host interfaces for LAN access. Miniblue is unauthenticated; restrict
access to trusted test clients using network controls.

In the stack's environment-variable editor, set the **name** to
`FUNCTION_MOCK_RESPONSES` and paste this JSON into the **value** field:

```json
{"POST /api/retrieve":{"status":200,"body":{"answer":"Your synthetic plan covers preventive care at 100% in-network.","confidence":0.94,"sources":[{"document_id":"synthetic-benefits-2026","title":"Synthetic Benefits Guide","plan_year":2026}]}},"POST /api/benny/start":{"status":200,"body":{"conversation_id":"synthetic-conversation-001","milestone":"started"}},"POST /api/benny/retrieve":{"status":200,"body":{"answer":"Your synthetic deductible is $500.","confidence":0.91}},"POST /api/benny/end":{"status":200,"body":{"conversation_id":"synthetic-conversation-001","outcome":"resolved"}},"POST /api/benny/*":{"status":200,"body":{"status":"fixture"}}}
```

Do not include a `${...}` wrapper or outer quotes in that field. These are
illustrative response shapes, not a Benny contract. The file requires a
nonempty value and passes it into the container; no backend is configured.
All services are enabled. Responses are fixed, regardless of request contents,
and do not create conversation state.

The Compose file builds the local `miniblue:benny` image from this repository
instead of using the upstream published image. Deploy on a Docker Standalone
environment with Compose build support, not a Swarm stack. If your Portainer
version does not build Git stacks, clone or pull this repository on the Docker
host and run `docker build -t miniblue:benny .` before deployment. Use that
local image without re-pulling it, and rebuild it after code updates.
Redeploy/recreate the container after changing environment variables.

Verify from a LAN client, replacing the example address with your Docker host:

```bash
curl -i -X POST http://192.168.1.121:4566/api/retrieve \
  -H 'Content-Type: application/json' \
  -d '{"query":"What does my plan cover?"}'
```

Expect HTTP 200 and the synthetic preventive-care answer. A 501 means there is
no matching mock or backend. A 404 indicates the route is not registered;
check that the image was rebuilt with invocation support and that `sites` is
enabled if you use a service filter.

To forward unmatched invocations to a local Functions runtime, another mock, or
a non-production Azure Function App, set `FUNCTION_BACKEND_URL` to a fixed
HTTP(S) origin with optional path prefix. `/api/...` is appended. Optionally
set `FUNCTION_BACKEND_API_KEY` to the operator's `x-functions-key` and
`FUNCTION_BACKEND_TIMEOUT` to a positive Go duration up to `1m` (default
`10s`). Mocks take precedence. URL, query and header restrictions are enforced;
incoming bearer tokens, cookies, function keys and common credential query
parameters including `code` are not forwarded.

## Key Vault rotation fixtures

The Key Vault mock retains versions: each PUT to
`/keyvault/{vault}/secrets/{name}` returns the created version in the
`X-Miniblue-Secret-Version` response header. GET the latest at the same path,
GET a historical value at `.../{version}`, and list metadata (no values) at
`.../versions`. DELETE removes all versions permanently. This tests application
logic that reads rotated values, not real Key Vault access control, references,
soft delete, managed identity or rotation propagation.

Optionally set `KEYVAULT_BACKEND_URL` to a fixed vault origin and
`KEYVAULT_BACKEND_BEARER_TOKEN` to an operator-provided token. HTTPS is required
except HTTP on loopback; the token is required and must be refreshed externally.
All local vault names then address that single backend vault, and operations
use Azure `/secrets/...` paths. Callers must provide an appropriate
`api-version`. `KEYVAULT_BACKEND_TIMEOUT` defaults to `10s`, up to `1m`.
Incoming credentials are not forwarded. To simulate errors, use a dedicated
mock backend. ARM Key Vault references are not automatically resolved.

## Application Insights, Log Analytics and Monitor

`SERVICES=monitor` enables local ARM create/get/list/delete configuration
stubs for `Microsoft.Insights/components`, `Microsoft.OperationalInsights/workspaces`,
`Microsoft.Insights/actionGroups`, `Microsoft.Insights/metricAlerts`, and
`Microsoft.Insights/workbooks`. They store metadata only. They do **not** run
queries, evaluate alerts, send notifications, render dashboards, ingest Azure
telemetry or enforce retention.

The separate simplified test record API is not the Azure ingestion API:

```bash
BASE=http://localhost:4566
curl --fail-with-body -X POST "$BASE/monitor/records" \
  -H 'Content-Type: application/json' \
  -d '{"conversation_id":"synthetic-1","timestamp":"2026-01-01T00:00:00Z","type":"outcome","data":{"milestone":"started"}}'
curl --fail-with-body "$BASE/monitor/records?conversation_id=synthetic-1"
curl --fail-with-body -X DELETE \
  "$BASE/monitor/records?conversation_id=synthetic-1&before=2026-01-08T00%3A00%3A00Z"
```

POST requires `conversation_id`, RFC3339 `timestamp`, `type` (`telemetry`
or `outcome`) and an object-valued `data`. DELETE removes records for a single
conversation, optionally strictly before the provided cutoff. To test seven-day
deletion, your application or an external scheduled process must calculate a
cutoff and perform deletion; there is **no automatic TTL**. This API cannot
prove deletion or retention compliance: deletion is not transactional, and the
shared store cannot report all read or deletion failures. Records are limited to 16 KiB, 1000 per
conversation, and 1 MiB per list response. State uses the configured store and
is in memory by default.

`MONITOR_BACKEND_URL` optionally forwards these three custom record operations
to the same `/monitor/records` path on a fixed backend; it is **not** an Azure
Monitor passthrough protocol. `MONITOR_BACKEND_API_KEY` optionally supplies its
`api-key` header. ARM configuration always stays local.

## Security and backend limits

Miniblue does not authenticate clients. Anyone who can reach it can read or
write mock secrets/records, invoke configured backends with operator credentials,
incur charges or affect real backend data. Restrict access to trusted test
clients, use isolated non-production resources and least-privileged keys, and
do not commit keys/tokens. TLS is required for the Key Vault backend except on
loopback; use HTTPS for other backends when carrying sensitive information.

Passthrough sends request bodies and permitted query values to the configured
destination; credential stripping is not PII scrubbing. The structured request
logger omits query strings but logs paths and client addresses; avoid putting
sensitive data in paths. Backend requests are timed out, redirects rejected,
and traffic bounded. No passthrough provides automatic token refresh or Azure
SDK protocol parity.
