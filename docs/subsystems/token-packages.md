# Lifetime token packages and quota console

The Docker-oriented quota console manages named, model-limited packages of
lifetime tokens, with a separate optional allowance for MCP search and webpage calls. It reuses Magpie's provider catalog, gateway identity and
protocol translation. It adds no production dependency.

## Sources of truth

- `internal/tokenpack` owns the persistent SQLite allowance and request ledger,
  package metadata, share credentials, reservations and administrative settlement.
  `mcp_calls.go` owns the independent accepted-tool-call counter and request table.
- `internal/access` identifies package-issued gateway keys explicitly, so losing
  the package database cannot silently turn one into an unrestricted gateway key.
- The gateway's package middleware reserves and settles paid generation requests;
  existing ordinary keys and periodic budgets retain their behavior.
- `internal/gui/token_packs.go` provides the focused management API, console assets
  and public read-only share page. `assets/quota/` contains its isolated UI.
- `internal/gui/quota_providers.go` provides narrow supplier summaries, validated
  configuration edits, draft model discovery and connection tests. Draft calls
  use a per-operation HTTP client selected through `catalog.WithHTTPClient`.
- `internal/gui/quota_mcp*.go` exposes the same management operations as fixed MCP
  tools. It dispatches in-process to the existing HTTP handlers, without a second
  storage or validation implementation.
- The gateway's dedicated package MCP at `gateway.PackageMCPPath` exposes only
  `get_quota`, `list_models` and `generate_text` to package-issued keys. Packages with
  a positive call allowance also receive `web_search` and `web_fetch`. The public
  share page serializes that endpoint and the recipient's package credential into
  a copyable HTTP MCP configuration; it never supplies management credentials.
- `internal/gui/quota_searches.go` exposes saved search service summaries and
  validated create/partial-update operations; `assets/quota/searches.js` embeds
  this setup in the existing supplier view. Raw search API keys stay server-side.
- `compose.yaml` selects the console using `MAGPIE_QUOTA_PANEL=1`. Upstream
  providers can be configured in the console, over management MCP, or with the CLI.

## Boundaries and state

The console has overview, package management, request logs and supplier setup. The administrator
uses the existing `MAGPIE_WEB_KEY` browser authentication. Other desktop and plugin
management routes are absent from the focused handler. The default desktop UI is
unchanged when this mode is not selected.

Supplier API responses contain no full API keys. Simple API-key suppliers can be
created, edited and enabled or disabled. Existing multi-key, multi-protocol,
account-backed or otherwise advanced suppliers are read-only here. Leaving an
existing key blank preserves it only for the same normalized complete base URL
and protocol. Changing either requires a new explicit key. Draft model discovery
and probes never save configuration, never follow redirects, use a 15-second
deadline and read at most 1 MiB from the chosen origin. Saving updates the package
model catalog without restarting. A probe makes a small generation request and
can incur upstream charges.

Management MCP is available only at exact `/api/mcp`, outside the browser cookie
guard but inside the web allow list. It requires a separate `MAGPIE_MCP_KEY` bearer
credential of 32–256 URL-safe ASCII characters, distinct from the browser key.
Missing, invalid or reused browser keys disable the endpoint. Cookies and package
keys do not authenticate MCP. Cross-origin browser requests are rejected.
The stateless Streamable HTTP transport supports `2025-11-25`, `2025-06-18` and
`2025-03-26`; unsupported revisions return explicit supported-version information.
Dual-era clients may fall back; clients pinned to `2026-07-28` are unsupported.
It returns JSON to POST requests, acknowledges notifications without executing
tools, and returns 405 to GET/DELETE. Tool arguments are bounded to 64 KiB and
whitelisted; captured responses are bounded to 4 MiB. There are no arbitrary HTTP
or shell tools. Administrative share links are returned intentionally, so the
management credential must never be distributed with a package.

Package MCP is a separate stateless Streamable HTTP endpoint at exact
`/mcp/magpie/package`, authenticated by the package API key, rather than the
management key or browser cookie. It supports initialize-era versions
`2025-11-25`, `2025-06-18` and `2025-03-26`, accepts POST requests and creates no
session or independent GET/SSE stream. Read-only quota and model tools do not
spend generation tokens. `generate_text` accepts `model`, `prompt`, optional
`system` and optional `maxTokens` (4096 by default), and uses the package's model
allow-list and the same durable reservation and usage settlement as its model API.
Its `usage.complete` describes completed accounting; unknown usage retains the
reservation. A null `expiresAt` in the quota result means no configured expiry.

This endpoint does not relay the administrator's MCP connection, plugin tools,
Library Hub or stored third-party MCP sign-ins. Search and webpage calls use only
the two explicitly exposed tools and their own package call allowance. Image
generation and other tools remain excluded. Enabling
package MCP never removes the package middleware's guards from other MCP routes.

Search configuration supports Tavily, Brave, Exa, Firecrawl, You.com, DeepSeek and
SearXNG. Only `web_search` uses these services; `web_fetch` directly reads public
pages through the existing guarded fetch path. Vendor defaults are supplied by the backend; SearXNG requires a supplied
base URL and supports private HTTP. Optional-key behavior follows each vendor.
Saved keys are never returned. Reusing a configured key requires the same full
normalized URL; changing that URL requires a new key. Advanced multi-key or
non-default key-selection settings remain read-only in this focused editor.
`PATCH` sends only fields the administrator changes.

Only exact public `/share/{token}` reads bypass administrator authentication.
Share tokens are independent bearer credentials: possession grants access to that
package's API key, allowed models and separate token/call balances. They never contain the administrator
key or an upstream provider key. Disabled or expired packages reveal no API key.
The base URL comes from `MAGPIE_PUBLIC_URL`, never the request's Host header.
Share responses forbid caching, referrer forwarding and framing.
Their MCP JSON is built with `encoding/json`, rendered as escaped HTML text and
copied with the page's existing clipboard controls. It contains only the public
package MCP URL and that package's key; separate package links cannot expose each
other's credentials.

An allowance counts input and output tokens once, including cache reads and
reasoning according to the upstream usage record. One million is 1,000,000 tokens.
A request durably reserves the package's available balance before forwarding;
requests of the same package run serially. A known final usage settles the charge
and releases the unused reservation. An interrupted or unknown result retains its
reservation for an administrator to reconcile. A restart does not release it.
Administrative settlement is idempotent and must not undercut a known partial
charge. Display logs are not the source of the allowance balance.

The ledger owns `mcpTotalCalls`, `mcpUsedCalls` and `mcpRemainingCalls` separately
from generation tokens. A missing call allowance defaults to zero and does not
disable model MCP. The UI edits a total, never an increment. After syntax,
entitlement and required-service validation, each accepted search or webpage call
atomically spends one durable call before executing. A logical call spends once
even if a search falls back between suppliers. Network failures, timeouts, later
public-target rejection, interruption and restart do not refund
it. Invalid arguments do not spend calls. No conversion to tokens occurs.
Call logs expose `unit: "calls"`, `calls` and the specific `tool`; they show an
explicit count in the UI and cannot enter the token settlement flow.
The version-1 ledger migration atomically adds the call counters and call-request
table. Existing packages start with zero tool calls; existing token balances,
reservations and gateway credentials are preserved. Missing or broken migrated
tables fail operations instead of silently recreating an allowance.

Search calls use a checked snapshot of enabled search API configuration, including
the exact credentials used for both dispatch and result redaction. They never
fall back to the administrator's search model. Both web tools share the gateway's
memory admission: each executing request holds 32 MiB through RPC encoding and
HTTP output. A single 30-second deadline covers queueing, execution and writing;
queue failure happens before a call is charged. Request-body reads have a separate
absolute 30-second deadline. Vendor bodies and complete RPC output are bounded to
4 MiB. These controls apply to the package endpoint, not ordinary MCP clients.

Package generation through MCP is logged with the fixed package MCP endpoint;
the focused log API exposes `source: "mcp"` and `tool: "generate_text"` when a
generation's model is identified. Metadata refusals have no invented tool name.
The quota console labels these rows MCP without retaining raw RPC arguments.

Request logs include zero-charge refusals as well as admitted requests. Only
fixed rejection reasons and approved model identifiers are kept; request bodies,
upstream error bodies and credentials are absent. The focused API presents
settled entries as completed or failed according to their HTTP status.

This prevents concurrent requests from reusing a balance. It does not promise an
exact universal hard cap for arbitrary provider tokenizers: one accepted request
may consume more tokens than remain. Unsupported paid operations fail closed.
Missing input or output counts, including ambiguous explicit zero counters, retain
the reservation for reconciliation. Package text request bodies are limited to
1 MiB after decoding and still use the gateway's shared memory admission. Normal
keys keep their existing protocol support and budget behavior.

## Verification

Tests use temporary configuration homes, cached Go modules and local fake upstreams.
Run `go test -tags nogui ./internal/tokenpack ./internal/gui ./internal/gateway`
and the package/gateway race tests. `node --test
internal/gui/tests/quota-console.test.cjs` exercises the real standalone console in
Chromium and WebKit. The full build and Go suite commands are in
`docs/plans/2026-10-30-token-packages.md`; deployment instructions and operational
limits are in `docs/token-packages.md`.
