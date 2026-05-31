# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Vault is a permanent, deduplicated storage layer for the Webtor platform (distributed torrent streaming). It exposes HTTP APIs to queue, store, and serve files, backed by PostgreSQL and S3-compatible object storage. Background workers process store/delete jobs asynchronously. NATS is used for inter-service event publishing.

## Build & Run

```bash
# Build (generates Swagger docs + compiles binary)
make build

# Run locally (requires PG, S3, and optionally NATS)
./vault serve

# Generate Swagger docs only
swag init -g services/web.go --instanceName vault
```

There are no tests in this repository currently.

## Architecture

**Entry point:** `main.go` → `configure.go` → `serve.go`

The `serve` command initializes all components in order: PG connection → migrations → probe/pprof → S3 client → Web server → REST API client → NATS → Worker → EventHandler. All implement `cs.Servable` and are managed by `common-services.Serve`.

**`services/` package — all core logic lives here:**

| File | Role |
|------|------|
| `web.go` | Gin HTTP server, routing, Swagger UI, error-to-status-code mapping |
| `resource.go` | PUT/GET/DELETE `/resource/{id}` handlers — queue storage or deletion (202 Accepted) |
| `webseed.go` | GET/HEAD `/webseed/{id}/{path}` — resolves `(id, path)` → S3 object hash; HEAD calls S3 directly, GET redirects to `${S3_CACHE_URL}/{hash}` if configured (single-tenant — bucket fixed in s3-cache config), else returns a presigned S3 URL |
| `models.go` | Data models (`Resource`, `File`, `ResourceFile`, `OperationLog`) and go-pg DB helpers |
| `worker.go` | Background job processor — polls DB every 5s, downloads from REST API, multipart-uploads to S3, publishes NATS events |
| `api.go` | Client for Webtor's external REST API with JWT auth — fetches torrent content lists and downloads file ranges |
| `event_handler.go` | JetStream pull subscriber on `resource.banned` (published by abuse-store on illegal-content reports) — funnels banned infohashes into `ResourceQueueForDeletion` so the worker tears them down. Skipped when NATS or PG is not configured |

**Key design patterns:**
- All store/delete operations are **asynchronous** — API returns 202, worker processes later
- **Deduplication** via content-hash-based `File` records shared across `Resource`s
- Worker uses **`FOR UPDATE SKIP LOCKED`** for safe concurrent job claiming
- Resource status lifecycle: `queued_for_storing` → `storing` → `stored` (or `store_error`), `queued_for_deletion` → `deleting` → (deleted or `delete_error`)
- Error wrapping with `pkg/errors` throughout worker code

## Database

PostgreSQL with auto-migrations via `go-pg/migrations`. Migration files in `migrations/` (5 migrations).

Core tables: `resource` (status, sizes, timestamps), `file` (hash-based PK, dedup), `resource_file` (junction with path), `log` (operation audit trail).

## Configuration

All config via CLI flags or environment variables (urfave/cli). Key groups:

- **Web:** `WEB_HOST`, `WEB_PORT` (default 8080), `S3_CACHE_URL` (when set, `/webseed` GET redirects through s3-cache instead of presigned S3; empty = legacy presigned path, rollback is a single env unset)
- **PostgreSQL:** `PG_HOST`, `PG_PORT`, `PG_USER`, `PG_PASSWORD`, `PG_DB`
- **S3:** `S3_ENDPOINT`/`AWS_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY`/`AWS_ACCESS_KEY_ID`, `S3_SECRET_KEY`/`AWS_SECRET_ACCESS_KEY`
- **Worker:** `WORKERS` (default 10), `AWS_UPLOAD_CONCURRENCY` (default 1), `AWS_UPLOAD_PART_SIZE` (default 50MB), `RESOURCE_ID` (debug single resource)
- **REST API:** `REST_API_SERVICE_HOST`, `REST_API_SERVICE_PORT`, `REST_API_SECURE`, `WEBTOR_API_KEY`, `WEBTOR_API_SECRET`
- **NATS/Probe/Pprof:** registered via `common-services`

## Dependencies

- **HTTP:** `gin-gonic/gin` with `swaggo/gin-swagger`
- **DB:** `go-pg/pg/v10` ORM
- **S3:** `aws/aws-sdk-go`
- **Messaging:** `nats-io/nats.go`
- **Shared infra:** `webtor-io/common-services` (PG, S3, NATS, probe, pprof flag registration and initialization)
- **Logging:** `sirupsen/logrus`
- **Errors:** `pkg/errors`

## Deployment

Docker multi-stage build (Alpine). Published to GHCR via GitHub Actions on push to `main` or version tags (`v*`). Helm chart symlinked at `chart/`.
