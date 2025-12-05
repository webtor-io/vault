# Vault

Permanent, deduplicated storage layer for Webtor.

## Quick start

Build (generates Swagger and binary):

```bash
make build
```

Run (env example):

```bash
WEB_PORT=8080 \
PG_HOST=127.0.0.1 PG_PORT=5432 PG_USER=postgres PG_PASSWORD=postgres PG_DB=vault \
S3_ENDPOINT=http://127.0.0.1:9000 S3_REGION=us-east-1 S3_BUCKET=vault S3_ACCESS_KEY=minio S3_SECRET_KEY=miniosecret \
./vault serve
```

Swagger UI: http://localhost:8080/swagger/index.html

## Configuration

- Web: `WEB_HOST` (default: empty), `WEB_PORT` (default: 8080)
- Postgres: `PG_HOST`, `PG_PORT`, `PG_USER`, `PG_PASSWORD`, `PG_DB`
- S3: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`

More flags (health/pprof/metrics, etc.) are provided by common-services.

## API (short)

- PUT `/resource/{id}` — queue store, returns 202 with resource
- GET `/resource/{id}` — fetch resource or 404
- DELETE `/resource/{id}` — queue delete or cancel queued store
- GET/HEAD `/webseed/{id}/{path}` — serve stored file with Range support

## License

See LICENSE.
