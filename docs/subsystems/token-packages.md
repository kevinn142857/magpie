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
- `compose.yaml` selects the console using `MAGPIE_QUOTA_PANEL=1`. Upstream
  providers are configured with the existing Magpie CLI.

## Boundaries and state

The console has overview, package management and request logs. The administrator
uses the existing `MAGPIE_WEB_KEY` browser authentication. Other desktop and plugin
management routes are absent from the focused handler. The default desktop UI is
unchanged when this mode is not selected.

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
