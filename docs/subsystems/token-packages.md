# Lifetime token packages and quota console

The Docker-oriented quota console manages named, model-limited packages of
lifetime tokens. It reuses Magpie's provider catalog, gateway identity and
protocol translation. It adds no production dependency.

## Sources of truth

- `internal/tokenpack` owns the persistent SQLite allowance and request ledger,
  package metadata, share credentials, reservations and administrative settlement.
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

Only exact public `/share/{token}` reads bypass administrator authentication.
Share tokens are independent bearer credentials: possession grants access to that
package's API key, allowed models and balance. They never contain the administrator
key or an upstream provider key. Disabled or expired packages reveal no API key.
The base URL comes from `MAGPIE_PUBLIC_URL`, never the request's Host header.
Share responses forbid caching, referrer forwarding and framing.

An allowance counts input and output tokens once, including cache reads and
reasoning according to the upstream usage record. One million is 1,000,000 tokens.
A request durably reserves the package's available balance before forwarding;
requests of the same package run serially. A known final usage settles the charge
and releases the unused reservation. An interrupted or unknown result retains its
reservation for an administrator to reconcile. A restart does not release it.
Administrative settlement is idempotent and must not undercut a known partial
charge. Display logs are not the source of the allowance balance.

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
