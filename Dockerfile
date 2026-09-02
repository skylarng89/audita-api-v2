# Audita API — multi-stage build (blueprint ADR-9).
# Build:  docker build -t audita-api .
# Run:    docker run --rm -it --env-file .env -p 8080:8080 audita-api

# ── Build stage ─────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS builder

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build all commands; migrations are embedded via the file path baked into
# the image (see runtime stage). CGO is disabled for a fully static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/audita-api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/audita-migrate ./cmd/migrate

# ── Runtime stage ───────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/audita-api /app/audita-api
COPY --from=builder /out/audita-migrate /app/audita-migrate
COPY --from=builder /src/internal/migrations /app/internal/migrations

# 12-factor: all configuration is env-var driven (DATABASE_URL, JWT_*,
# NATS_URL, SMTP_*, STORAGE_*, OTEL_*). No secrets are baked into the image.
ENV PORT=8080 \
    MIGRATIONS_PATH=/app/internal/migrations

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/app/audita-api"]
