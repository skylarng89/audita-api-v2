// Command api runs the audita-api monolith binary: it wires all modules into
// one chi router, starts background jobs (outbox relay, SLA scheduler, audit
// verification), and serves HTTP + WebSocket endpoints (blueprint §3.2).
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/audita/audita-api/internal/config"
	"github.com/audita/audita-api/internal/httpx"
	"github.com/audita/audita-api/internal/identity"
	"github.com/audita/audita-api/internal/platform/db"
	"github.com/audita/audita-api/internal/platform/eventbus"
	"github.com/audita/audita-api/internal/platform/health"
	"github.com/audita/audita-api/internal/platform/runtime"
	"github.com/audita/audita-api/internal/tenancy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5"
)

func main() {
	logger := runtime.NewLogger(getEnvOrDefault("LOG_LEVEL_APP", "info"))
	cfg, err := config.Load()
	if err != nil {
		runtime.Fatalf(logger, "config load: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Database ────────────────────────────────────────────────────────
	if err := db.WaitForDB(ctx, cfg.DatabaseURL, 60*time.Second, logger); err != nil {
		platform.Fatalf(logger, "database not reachable: %v", err)
	}
	pool, err := db.ConnectPool(ctx, cfg.DatabaseURL, 20)
	if err != nil {
		runtime.Fatalf(logger, "connect db: %v", err)
	}
	defer pool.Close()
	if err := db.MigrateUp(cfg.DatabaseURL, "./internal/migrations"); err != nil {
		platform.Fatalf(logger, "migrate: %v", err)
	}
	tenantDB := runtime.NewTenantDB(pool, 10*time.Second)

	// ── Event bus + outbox relay ────────────────────────────────────────
	bus, err := eventbus.Connect(ctx, cfg.NatsURL, logger)
	if err != nil {
		logger.Warn("NATS unavailable; starting with outbox-only event delivery", slog.String("error", err.Error()))
	}
	if bus != nil {
		if err := bus.EnsureStream(ctx); err != nil {
			logger.Warn("NATS stream ensure failed", slog.String("error", err.Error()))
		}
		defer func() { _ = bus.Close() }()
		relay := eventbus.NewOutboxRelay(pool, bus, logger)
		go relay.Run(ctx, 2*time.Second)
	}

	// ── Modules ─────────────────────────────────────────────────────────
	tenancySvc := tenancy.NewService(pool)
	tenantTxRunner := func(ctx context.Context, fn func(tx pgx.Tx) error) error {
		userID, _ := httpx.UserID(ctx)
		role, _ := httpx.Role(ctx)
		return tenantDB.InTenantTx(ctx, runtime.TenantContext{UserID: userID, Role: role}, fn)
	}
	tokens, err := identity.NewTokenService(cfg.JWTPrivateKeyPEM, cfg.JWTAccessTTL)
	if err != nil {
		runtime.Fatalf(logger, "token service: %v", err)
	}
	identitySvc := identity.NewService(tenantTxRunner, tokens, cfg.RefreshTokenTTL)
	identityHandler := identity.NewHandler(identitySvc, tenantTxRunner, time.Duration(cfg.InviteExpiryHours)*time.Hour)
	auth := identity.NewAuthenticator(tokens)
	tenancyHandler := tenancy.NewHandler(tenancySvc)

	// ── Router ──────────────────────────────────────────────────────────
	r := chi.NewRouter()
	for _, mw := range runtime.Middleware(logger) {
		r.Use(mw)
	}
	if cfg.MetricsEnabled {
		metrics := runtime.NewMetrics()
		r.Use(metrics.MetricsMiddleware)
		r.Handle("/metrics", metrics.Handler())
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSAllowedOris,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Tenant-Slug", "X-Request-ID", "Idempotency-Key"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Platform (cross-tenant) routes — super-admin gated in Sprint 5.
	r.Route("/api/platform/v1", func(pr chi.Router) {
		tenancyHandler.Routes(pr)
	})

	// Tenant-scoped routes.
	r.Route("/api/v1", func(tr chi.Router) {
		tr.Use(tenancy.Middleware(tenancySvc))
		identityHandler.Routes(tr) // auth endpoints (no auth required)
		tr.Group(func(ar chi.Router) {
			ar.Use(auth.Middleware)
			// Sprint 2-5 module routers mount here (changerequest,
			// collaboration, notification WS, audit).
			ar.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
				uid, _ := httpx.UserID(r.Context())
				role, _ := httpx.Role(r.Context())
				httpx.RespondJSON(w, http.StatusOK, map[string]string{"userId": uid.String(), "role": role})
			})
		})
	})

	// Health endpoints.
	healthHandler := health.NewHandler(map[string]health.Checker{
		"postgres": func(ctx context.Context) error { return tenantDB.Ping(ctx) },
		"nats": func(ctx context.Context) error {
			if bus == nil {
				return nil // degraded-but-alive: outbox still buffers
			}
			if !bus.IsConnected() {
				return errNatsDisconnected
			}
			return nil
		},
	})
	healthHandler.Register(r)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("audita-api listening", slog.String("port", cfg.Port))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			runtime.Fatalf(logger, "http server: %v", err)
		}
	}
	logger.Info("audita-api stopped")
}
