# Audita API (v2)

Go backend for Audita v2 — a modular monolith (single binary, split-ready modules) serving REST + WebSocket endpoints over Postgres (RLS multi-tenancy) and NATS JetStream.

> Full architecture: [`../.dev/architectures/2026-09-01-audita-v2-microservice-rebuild.md`](../.dev/architectures/2026-09-01-audita-v2-microservice-rebuild.md)

## Commands

| Command                | Purpose                                                                                              |
| ---------------------- | ---------------------------------------------------------------------------------------------------- |
| `go run ./cmd/api`     | Monolith entrypoint: chi router, identity/tenancy routes, outbox relay, health endpoints, `/metrics` |
| `go run ./cmd/migrate` | One-shot golang-migrate job (idempotent; runs all pending SQL migrations)                            |
| `go run ./cmd/keygen`  | Generates `JWT_PRIVATE_KEY_PEM` / `JWT_PUBLIC_KEY_PEM` / `APP_ENCRYPTION_KEY` for `.env`             |

## Module layout

Bounded contexts under `internal/` — each owns its tables, migrations, and a narrow package API. Cross-module side effects flow through NATS events, never direct imports (ADR-1/FR-4, enforced by `depguard` rules in `.golangci.yml`).

```text
internal/
├── identity/        users, roles, JWT + refresh cookie, invites, password reset
│   ├── handler.go   /api/v1/auth/* routes
│   ├── middleware.go  Bearer-JWT auth + RequireRole + cookie helpers
│   ├── service.go   use-cases (login, refresh, invite, reset)
│   ├── store.go     tx-scoped SQL (runs inside RLS transactions only)
│   └── token.go     RS256 issue/verify, bcrypt, opaque-token hashing
├── tenancy/         tenants, slug→tenant resolution, platform CRUD
├── changerequest/   CR lifecycle (Sprint 2 — schema ready)
├── collaboration/   comments, mentions, attachments (Sprint 2)
├── audit/           hash-chained audit log (Sprint 4)
├── notification/    notifications + WebSocket gateway (Sprint 3)
├── platform/        shared infra (not a bounded context)
│   ├── runtime      TenantDB (RLS tx template), middleware chain, metrics, logging
│   ├── db/          pgxpool bootstrap + migrate helpers
│   ├── eventbus/    NATS JetStream wrapper + transactional outbox relay
│   ├── crypto/      AES-GCM at-rest secret encryption
│   ├── storage/     local-FS / S3 abstraction for attachments
│   ├── email/       SMTP sender (MailHog-compatible)
│   ├── idempotency/ Idempotency-Key store for mutating endpoints
│   ├── health/      /healthz + /readyz with per-component checks
│   └── dbtest/      FR-3 RLS negative-isolation test gate
├── httpx/           shared Problem (RFC7807-style) JSON decode/respond helpers
└── migrations/      golang-migrate SQL 0001–0007 (incl. RLS policies)
```

## Multi-tenancy & RLS

Every tenant-scoped table carries `tenant_id UUID NOT NULL` with a Postgres RLS policy:

```sql
USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
```

Application code MUST run tenant-scoped queries through `runtime.TenantDB.InTenantTx`, which sets `app.tenant_id`, `app.user_id`, and `app.role` via `set_config(..., true)` (transaction-scoped, auto-reset). Requests resolve the tenant via `X-Tenant-Slug` (or subdomain) in `tenancy.Middleware`, which runs before JWT validation.

`internal/platform/dbtest/rls_test.go` is the required CI gate: it seeds two tenants across all 26 tenant-scoped tables and asserts cross-tenant reads return zero rows. It skips when `TEST_DATABASE_URL` is unset.

## API surface (current)

All tenant-scoped routes require `X-Tenant-Slug`; auth routes additionally require `Authorization: Bearer <access-token>` except where noted.

| Method   | Path                                   | Auth     | Purpose                                           |
| -------- | -------------------------------------- | -------- | ------------------------------------------------- |
| POST     | `/api/v1/auth/login`                   | —        | Email/password → access token + refresh cookie    |
| POST     | `/api/v1/auth/refresh`                 | cookie   | Rotate refresh token, new access token            |
| POST     | `/api/v1/auth/logout`                  | cookie   | Revoke refresh token, clear cookie                |
| GET      | `/api/v1/auth/session`                 | Bearer   | Restore current user                              |
| POST     | `/api/v1/auth/forgot-password`         | —        | Request reset email (always 202)                  |
| POST     | `/api/v1/auth/reset-password`          | —        | Consume reset token, set new password             |
| POST     | `/api/v1/auth/accept-invite`           | —        | Activate pending user via invite token            |
| GET      | `/api/v1/whoami`                       | Bearer   | Debug identity echo (temporary)                   |
| GET/POST | `/api/platform/v1/tenants`             | platform | Tenant list/create (super-admin, Sprint 5 gating) |
| GET      | `/api/platform/v1/tenants/{id}`        | platform | Tenant detail                                     |
| POST     | `/api/platform/v1/tenants/{id}/status` | platform | Suspend/reactivate/archive                        |

Errors use a consistent JSON `Problem` shape (`code`, `message`, `details?`, `requestId`, `timestamp`); see `internal/httpx`.

## Configuration

12-factor env vars (defaults in parentheses); validation fails fast on missing required values:

| Variable                            | Required | Default / notes                                            |
| ----------------------------------- | -------- | ---------------------------------------------------------- |
| `PORT`                              | —        | `8080`                                                     |
| `DATABASE_URL`                      | ✔        | `postgres://user:pass@host:5432/audita?sslmode=disable`    |
| `NATS_URL`                          | —        | `nats://localhost:4222`                                    |
| `JWT_PRIVATE_KEY_PEM`               | ✔        | RSA private key (PKCS#1 or PKCS#8) — `go run ./cmd/keygen` |
| `JWT_PUBLIC_KEY_PEM`                | ✔        | Matching RSA public key                                    |
| `JWT_EXPIRY_SECONDS`                | —        | Access-token TTL, `900` (15 min)                           |
| `REFRESH_TOKEN_EXPIRY_DAYS`         | —        | Refresh TTL in seconds-equivalent, `1209600` (14 d)        |
| `APP_ENCRYPTION_KEY`                | ✔        | base64 ≥16 bytes (32 recommended) — AES-GCM envelope key   |
| `BOOTSTRAP_SETUP_TOKEN`             | —        | Gate for first-run platform bootstrap                      |
| `CORS_ALLOWED_ORIGINS`              | —        | Comma-separated, `http://localhost:7000`                   |
| `SMTP_HOST` / `SMTP_PORT`           | —        | `localhost` / `1025` (MailHog in compose)                  |
| `SMTP_FROM` / `SMTP_FROM_NAME`      | —        | `no-reply@audita.local` / `Audita`                         |
| `SMTP_AUTH` / `SMTP_TLS`            | —        | `false` / `false` (enable for real providers)              |
| `STORAGE_BACKEND`                   | —        | `local` (default) or `s3`                                  |
| `STORAGE_LOCAL_BASE_PATH`           | —        | `/tmp/uploads`                                             |
| `STORAGE_S3_*`                      | s3 only  | `BUCKET`, `REGION`, `ACCESS_KEY`, `SECRET_KEY`             |
| `UPLOAD_MAX_SIZE_BYTES`             | —        | `26214400` (25 MiB)                                        |
| `INVITE_EXPIRY_HOURS`               | —        | `72`                                                       |
| `SLA_WARN_HOURS_BEFORE`             | —        | `24`                                                       |
| `OTEL_EXPORTER_OTLP_ENDPOINT`       | —        | Empty disables tracing export                              |
| `OTEL_TRACING_SAMPLING_PROBABILITY` | —        | `0.1`                                                      |
| `LOG_LEVEL_APP`                     | —        | `debug`/`info`/`warn`/`error`                              |
| `METRICS_ENABLED`                   | —        | `true` (exposes `/metrics` for Prometheus)                 |

## Running locally

### With the full stack (recommended)

From the repo root (`audita-v2/`), `docker compose up --build` boots Postgres, NATS, the migrate job, the API, the SPA, and MailHog. See the root [README](../README.md#quick-start).

### API only against local Postgres

```bash
# 1. Secrets
go run ./cmd/keygen > keys.env   # paste values into your shell/.env

# 2. Migrations
go run ./cmd/migrate -url "$DATABASE_URL" -path ./internal/migrations

# 3. Serve
export DATABASE_URL=... JWT_PRIVATE_KEY_PEM=... JWT_PUBLIC_KEY_PEM=... APP_ENCRYPTION_KEY=...
go run ./cmd/api

# 4. Smoke-test
curl -s localhost:8080/healthz | jq
curl -s localhost:8080/readyz | jq
```

## Testing

```bash
go test ./...                                  # unit tests (RLS tests skip without TEST_DATABASE_URL)

# FR-3 RLS isolation gate — needs a migrated Postgres (compose `postgres` + `migrate`):
export TEST_DATABASE_URL="postgres://audita_app:...@localhost:5432/audita?sslmode=disable"
go test ./internal/platform/dbtest/... -v
```

## Quality gates

```bash
go vet ./...
golangci-lint run     # includes depguard import-boundary enforcement (ADR-1)
```

CI (`.github/workflows/audita-api.yml`) runs lint, tests (with a Postgres service for the RLS gate), a sqlc freshness check, `govulncheck`, and Trivy scans.

## Conventions

- **Module boundaries**: bounded contexts never import sibling `store.go` internals — communicate via NATS events or published port interfaces (lint-enforced).
- **SQL**: hand-written, tx-scoped functions (`*Tx(ctx, tx, ...)`) in each module's `store.go`; sqlc generation configured in `sqlc.yaml` for query-codegen going forward.
- **Errors**: return `*httpx.Problem` from services; handlers render via `httpx.RespondProblem` — never leak internals as 500s.
- **Migrations**: additive `NNNN_name.up.sql`/`.down.sql` pairs; tenant-scoped tables must be added to both `0007`-style scoping and the RLS test table list.
