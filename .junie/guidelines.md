Project: github.com/webtor-io/vault

Overview
- This service exposes an HTTP API for managing "resources" backed by Postgres and S3. Runtime wiring and ops come from github.com/webtor-io/common-services. The web layer is implemented with gin and instrumented with Swagger via swaggo.

Build and Configuration
1) Build artifacts
- Preferred: use the Makefile target which first (re)generates Swagger stubs and then builds the binary.
  Commands:
  - make build
    Internally runs: `swag init -g services/web.go && go build .`
  - The binary name is derived from `main` module; running `go build .` in repo root produces `vault`.

2) Toolchain notes
- go.mod declares `go 1.25`. Ensure your local Go toolchain is >= the declared version. If your local version lags, the code still builds on 1.22+ in practice, but align CI with module go version to avoid subtle behavior differences in stdlib (e.g., `maps/slices` packages).
- `swag` CLI must be available for `make build` (github.com/swaggo/swag). Install it once if missing: `go install github.com/swaggo/swag/cmd/swag@latest`.

3) Runtime configuration (CLI flags/env)
The service is started via `vault serve` (see `makeServeCMD` in serve.go). Flags are registered from common-services and the web package:
- Probe/health: provided by common-services.
- PPROF: provided by common-services.
- Postgres: provided by common-services. Typical env mapping includes DSN/host, port, user, pass, db name.
- S3 client: provided by common-services (endpoint, region, bucket, credentials).
- Web server (services.RegisterWebFlags):
  - host (env WEB_HOST) — default: empty string (binds all interfaces)
  - port (env WEB_PORT) — default: 8080

Example run (env driven):
```
WEB_PORT=8080 \
PG_HOST=127.0.0.1 PG_PORT=5432 PG_USER=postgres PG_PASSWORD=postgres PG_DB=vault \
S3_ENDPOINT=http://127.0.0.1:9000 S3_REGION=us-east-1 S3_BUCKET=vault S3_ACCESS_KEY=minio S3_SECRET_KEY=miniosecret \
./vault serve
```

Database Migrations
- `serve` runs Postgres migrations at startup via `common-services` (`m.Run()` in serve.go). The SQL lives in `migrations/` (currently `1_init.up.sql`/`down.sql`).
- For local development, ensure the configured DB is reachable and credentials are correct before starting the service. The service will apply new migrations automatically.

HTTP Surface
- Base path: `/`
- Resource endpoints (services/web.go):
  - PUT `/resource/{id}` → queues storing of a resource. Returns 202 with a `Resource` payload.
  - GET `/resource/{id}` → returns the resource or 404 (null semantics on DB miss handled in code).
  - DELETE `/resource/{id}` → queues deletion or deletes queued-for-storing.
  - ANY `/webseed/{id}/{path}` → placeholder handler; streams or proxies content as implemented.
- Swagger UI available at `/swagger/index.html`. The generator is anchored at `services/web.go` with `docs` package produced under `./docs`.

Testing
1) Running tests
- Standard: `go test ./...`
- Package-level: `go test ./services -run TestName`

2) Adding tests
- Use `_test.go` files alongside the code under test. Prefer black-box testing of exported functions in `services` and integration testing via HTTP gin router if feasible.
- Example subjects:
  - Flag registration and defaults (pure, no I/O) in `services.RegisterWebFlags`/`NewWeb`.
  - DB helpers in `services/models.go` can be tested with a real or ephemeral Postgres (e.g., testcontainers) if you add such infra; currently the project relies on a live DB configured via common-services, which is not mocked here.

3) Verified working example (was executed locally during preparation)
The following minimal test validates CLI flag defaults and overrides for the web service constructor. To try it yourself, create `services/web_test.go` with this content:
```
package services

import (
    "flag"
    "testing"
    "github.com/urfave/cli"
)

func TestNewWeb_Defaults(t *testing.T) {
    app := cli.NewApp()
    app.Flags = RegisterWebFlags(app.Flags)
    set := flag.NewFlagSet("test", flag.ContinueOnError)
    set.String("host", "", "")
    set.Int("port", 8080, "")
    ctx := cli.NewContext(app, set, nil)
    w := NewWeb(ctx, nil)
    if w.host != "" { t.Fatalf("expected empty host, got %q", w.host) }
    if w.port != 8080 { t.Fatalf("expected port 8080, got %d", w.port) }
}

func TestNewWeb_WithFlags(t *testing.T) {
    app := cli.NewApp()
    app.Flags = RegisterWebFlags(app.Flags)
    set := flag.NewFlagSet("test", flag.ContinueOnError)
    set.String("host", "", "")
    set.Int("port", 8080, "")
    _ = set.Set("host", "127.0.0.1")
    _ = set.Set("port", "9090")
    ctx := cli.NewContext(app, set, nil)
    w := NewWeb(ctx, nil)
    if w.host != "127.0.0.1" { t.Fatalf("expected host 127.0.0.1, got %q", w.host) }
    if w.port != 9090 { t.Fatalf("expected port 9090, got %d", w.port) }
}
```
Run:
```
go test ./services -run TestNewWeb
```
This test was executed successfully during preparation and then removed per task requirement.

Development Notes / Conventions
- Code style follows standard Go formatting (`gofmt`/`goimports`) and idiomatic naming. Keep package-local types unexported unless used outside the package. Align struct tags with go-pg `pg:"..."` and JSON as in `services/models.go`.
- Logging uses logrus with a text formatter enabled in `main.go`. Prefer structured logs (`WithField(s)`) for operational events. Avoid logging secrets.
- Error handling uses `github.com/pkg/errors` for wrapping in the web layer; prefer `errors.Is`/`errors.As` with sentinel errors like `pg.ErrNoRows` in DB helpers (see `ResourceGetByID`).
- Database layer is intentionally thin: helpers live next to models in `services/models.go`. Avoid introducing a separate repository abstraction unless needed; if expanding, mirror the existing helpers’ style (context passing, `WherePK`, `Select`, `Update`, and `Insert`).
- Migrations must reflect the `pg` mappings exactly (see comments in `models.go`). Any schema change should update both migration SQL and struct tags.
- Swagger annotations: keep endpoint comments above handlers in `services/web.go` using swaggo tags already present. After changes, re-run `make build` to refresh docs.
- Lifecycle: `serve` constructs services via common-services and defers `Close` calls. When adding a new service, implement the `cs.Servable` interface and append it before the `Serve` aggregator.

Local Troubleshooting
- If `make build` fails on `swag` not found: install it (see Toolchain notes).
- If migrations fail at startup, verify PG connectivity and credentials. The service will exit with a logged error; run with higher log verbosity if needed.
- If Swagger UI returns 404, ensure `swag init` ran (use `make build`) and the `docs` package is present.

Appendix: Useful commands
- Clean build cache: `go clean -testcache`
- Run only services tests: `go test ./services -v`
- Generate Swagger without building: `swag init -g services/web.go`
