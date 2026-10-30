# Token Packages Implementation Plan

**Goal:** Run the Magpie fork in Docker with a focused quota console and shareable, model-limited token packages.

**Architecture:** Reuse Magpie's providers, protocol translation, gateway identities and web authentication. A separate SQLite package ledger persists lifetime usage and reservations independently of display logs. `MAGPIE_QUOTA_PANEL=1` selects a small console exposing overview, package management and request logs; exact public share routes bypass only administrator authentication.

**Tech Stack:** Existing Go, modernc SQLite, embedded HTML/CSS/JavaScript and Playwright; no new production dependencies.

## Approved scope

- Create a package by name, allowed model IDs, total tokens and optional expiration.
- Generate a dedicated gateway API key and an independent random share token.
- Display base URL, key, models and remaining tokens to the share-link holder.
- Manage total quota and enabled status, and show request logs without prompts or secrets.
- Reuse the gateway and isolate providers/other desktop functions from the quota console. Configure upstream providers through Magpie's existing CLI.
- Define 1M as 1,000,000 input plus output tokens, including cached tokens once. Preserve uncertain reservations on missing final usage; document that cross-provider input estimation prevents a universal exact hard cap.

## Tasks and ownership

1. **Persistent ledger and gateway enforcement.** Add `internal/tokenpack/` and integrate reservation/settlement into gateway request lifecycles. Test creation, validation, model scope, concurrency, expiration, disable, persistent spend, missing usage and replay-safe settlement before implementation. Run package and gateway tests under isolated homes and race detection.
2. **Focused console.** Add `internal/gui/assets/quota/` with overview, quota management and request logs. Test creation, adjustment, disabling, copying links, empty/error states, literal untrusted values, keyboard access and mobile layout in Chromium and WebKit, with locale coverage.
3. **Management API and public share page.** Add `internal/gui/token_packs.go` and share template, route the quota-only handler from `Handler`, and mount exact public share paths outside the web-key guard. Test authentication boundaries, payload validation, correct public URLs and restricted routes; never use the administrator login URL as a package link.
4. **Deployment.** Add `compose.yaml`, `.env.example` and `docs/token-packages.md`; reuse `Dockerfile`, named `/config` volume and single web/API port. Validate interpolation, build and run with sandbox configuration and a local fake upstream.
5. **Review and verification.** Update subsystem references; inspect all changes; run formatting, Go vet/type compilation, native/Linux/Windows builds, full nogui tests, relevant race tests and both browser engines. Record unavailable or failing checks explicitly and resolve failures relevant to the change.

## Verification commands

Pin `GOPATH`, `GOMODCACHE` and `GOCACHE` before substituting a temporary HOME. Tests and smoke runs must never reach live agent configuration or real provider keys.

```sh
gofmt -l <changed Go files>
go vet ./...
GOOS=linux go vet -tags nogui ./...
GOOS=windows go vet ./...
go build -o <temporary directory>/magpie .
GOOS=linux go build -tags nogui -o <temporary directory>/magpie-linux .
GOOS=windows go build -o <temporary directory>/magpie.exe .
go test -count=1 -timeout=30m -tags nogui ./...
go test -count=1 -race -tags nogui ./internal/tokenpack ./internal/gui ./internal/gateway
node --test internal/gui/tests/quota-console.test.cjs
docker compose config --quiet
docker compose build
```
