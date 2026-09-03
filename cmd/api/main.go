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

	"github.com/audita/audita-api/internal/audit"
	"github.com/audita/audita-api/internal/changerequest"
	"github.com/audita/audita-api/internal/collaboration"
	"github.com/audita/audita-api/internal/config"
	"github.com/audita/audita-api/internal/httpx"
	"github.com/audita/audita-api/internal/identity"
	"github.com/audita/audita-api/internal/notification"
	"github.com/audita/audita-api/internal/orgstructure"
	"github.com/audita/audita-api/internal/platform/db"
	"github.com/audita/audita-api/internal/platform/eventbus"
	"github.com/audita/audita-api/internal/platform/health"
	"github.com/audita/audita-api/internal/platform/otelx"
	"github.com/audita/audita-api/internal/platform/runtime"
	"github.com/audita/audita-api/internal/platform/storage"
	"github.com/audita/audita-api/internal/tenancy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := runtime.NewLogger(getEnvOrDefault("LOG_LEVEL_APP", "info"))
	cfg, err := config.Load()
	if err != nil {
		runtime.Fatalf(logger, "config load: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, cleanup := buildApp(ctx, cfg, logger)
	defer cleanup()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.router,
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

// app bundles the wired router plus the handles background jobs need.
type app struct {
	router *chi.Mux
}

// buildApp performs all startup wiring: database, event bus, module
// construction, route mounting, and background-job startup. It returns the
// HTTP router plus a cleanup func for graceful shutdown.
func buildApp(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*app, func()) {
	cleanups := make([]func(), 0, 4)
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	pool := connectDatabase(ctx, cfg, logger)
	cleanups = append(cleanups, pool.Close)
	tenantDB := runtime.NewTenantDB(pool, 10*time.Second)

	if tp, err := otelx.Setup(ctx, "audita-api", cfg.OTLPEndpoint, cfg.OTLPSamplingProb, logger); err != nil {
		logger.Warn("otel setup failed; tracing disabled", slog.String("error", err.Error()))
	} else {
		cleanups = append(cleanups, func() { _ = tp.Shutdown(context.Background()) })
	}

	bus := connectEventBus(ctx, cfg, pool, logger, &cleanups)

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

	// Tenant-context bridges (module-level injectors, avoids cross-module imports).
	changerequest.SetTenantLookup(httpx.TenantID)
	collaboration.SetTenantLookup(httpx.TenantID)
	notification.SetUserLookup(httpx.UserID)

	store, err := storage.New(cfg.StorageBackend, cfg.StorageLocalBasePath,
		cfg.StorageS3Bucket, cfg.StorageS3Region, cfg.StorageS3AccessKey, cfg.StorageS3SecretKey)
	if err != nil {
		runtime.Fatalf(logger, "storage backend: %v", err)
	}

	metrics := runtime.NewMetrics()
	crSvc := changerequest.NewService(tenantTxRunner, changerequest.NewEventPublisher(pool, logger))
	crHandler := changerequest.NewHandler(crSvc)
	collabSvc := collaboration.NewService(tenantTxRunner, sanitizeHTML, collaboration.NewEventPublisher(pool, logger))
	collabHandler := collaboration.NewHandler(collabSvc, store)
	gateway := notification.NewGateway(cfg.CORSAllowedOris, wsMetricsAdapter{metrics}, logger)
	notifSvc := notification.NewService(tenantTxRunner, gateway)
	notifHandler := notification.NewHandler(notifSvc, gateway)
	notifHandler.BindWSHandler(gateway.HandleUpgrade(wsTokenAdapter{tokens}))
	auditSvc := audit.NewService(tenantTxRunner)
	auditHandler := audit.NewHandler(auditSvc)
	orgHandler := orgstructure.NewHandler(orgstructure.NewService(tenantTxRunner))

	router := buildRouter(routerDeps{
		cfg: cfg, logger: logger, metrics: metrics, tenancySvc: tenancySvc,
		tenancyHandler: tenancyHandler, identityHandler: identityHandler, auth: auth,
		crHandler: crHandler, collabHandler: collabHandler, notifHandler: notifHandler,
		auditHandler: auditHandler, orgHandler: orgHandler, tenantDB: tenantDB, bus: bus,
	})

	startBackgroundJobs(bgDeps{
		ctx: ctx, cfg: cfg, logger: logger, pool: pool, bus: bus,
		notifSvc: notifSvc, auditSvc: auditSvc, tenantTxRunner: tenantTxRunner, metrics: metrics,
	})
	return &app{router: router}, cleanup
}

// routerDeps bundles everything buildRouter mounts (keeps the function
// under the parameter-count lint limit).
type routerDeps struct {
	cfg             *config.Config
	logger          *slog.Logger
	metrics         *runtime.Metrics
	tenancySvc      *tenancy.Service
	tenancyHandler  *tenancy.Handler
	identityHandler *identity.Handler
	auth            *identity.Authenticator
	crHandler       *changerequest.Handler
	collabHandler   *collaboration.Handler
	notifHandler    *notification.Handler
	auditHandler    *audit.Handler
	orgHandler      *orgstructure.Handler
	tenantDB        *runtime.TenantDB
	bus             *eventbus.Bus
}

// bgDeps bundles background-job dependencies.
type bgDeps struct {
	ctx            context.Context
	cfg            *config.Config
	logger         *slog.Logger
	pool           *pgxpool.Pool
	bus            *eventbus.Bus
	notifSvc       *notification.Service
	auditSvc       *audit.Service
	tenantTxRunner changerequest.TenantTxRunner
	metrics        *runtime.Metrics
}

// connectDatabase waits for, connects to, and migrates Postgres.
func connectDatabase(ctx context.Context, cfg *config.Config, logger *slog.Logger) *pgxpool.Pool {
	if err := db.WaitForDB(ctx, cfg.DatabaseURL, 60*time.Second, logger); err != nil {
		runtime.Fatalf(logger, "database not reachable: %v", err)
	}
	pool, err := db.ConnectPool(ctx, cfg.DatabaseURL, 20)
	if err != nil {
		runtime.Fatalf(logger, "connect db: %v", err)
	}
	if err := db.MigrateUp(cfg.DatabaseURL, getEnvOrDefault("MIGRATIONS_PATH", "./internal/migrations")); err != nil {
		runtime.Fatalf(logger, "migrate: %v", err)
	}
	return pool
}

// connectEventBus establishes NATS (degraded mode without it) and starts
// the outbox relay. Appends its Close to the cleanup list.
func connectEventBus(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger, cleanup *[]func()) *eventbus.Bus {
	bus, err := eventbus.Connect(ctx, cfg.NatsURL, logger)
	if err != nil {
		logger.Warn("NATS unavailable; starting with outbox-only event delivery", slog.String("error", err.Error()))
		return nil
	}
	if err := bus.EnsureStream(ctx); err != nil {
		logger.Warn("NATS stream ensure failed", slog.String("error", err.Error()))
	}
	*cleanup = append(*cleanup, func() { _ = bus.Close() })
	relay := eventbus.NewOutboxRelay(pool, bus, logger)
	go relay.Run(ctx, 2*time.Second)
	return bus
}

// buildRouter assembles middleware, platform routes, tenant routes, and
// health endpoints.
func buildRouter(d routerDeps) *chi.Mux {
	r := chi.NewRouter()
	for _, mw := range runtime.Middleware(d.logger) {
		r.Use(mw)
	}
	if d.cfg.MetricsEnabled {
		r.Use(d.metrics.MetricsMiddleware)
		r.Handle("/metrics", d.metrics.Handler())
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   d.cfg.CORSAllowedOris,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Tenant-Slug", "X-Request-ID", "Idempotency-Key"},
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Route("/api/platform/v1", d.tenancyHandler.Routes)
	r.Route("/api/v1", func(tr chi.Router) {
		tr.Use(tenancy.Middleware(d.tenancySvc))
		d.identityHandler.Routes(tr)
		tr.Group(func(ar chi.Router) {
			ar.Use(d.auth.Middleware)
			ar.Get("/whoami", whoamiHandler)
			d.crHandler.Routes(ar)
			ar.Route("/change-requests/{id}", d.collabHandler.Routes)
			d.notifHandler.Routes(ar)
			d.auditHandler.Routes(ar)
			d.orgHandler.Routes(ar)
		})
	})

	healthHandler := health.NewHandler(map[string]health.Checker{
		"postgres": d.tenantDB.Ping,
		"nats": func(ctx context.Context) error {
			if d.bus == nil {
				return nil // degraded-but-alive: outbox still buffers
			}
			if !d.bus.IsConnected() {
				return errNatsDisconnected
			}
			return nil
		},
	})
	healthHandler.Register(r)
	return r
}

// startBackgroundJobs launches event consumers, the SLA monitor, audit
// verification, the export worker, and optional demo seeding.
func startBackgroundJobs(d bgDeps) {
	if d.bus != nil {
		if err := notification.NewConsumers(d.bus, d.notifSvc, d.logger).Subscribe(d.ctx); err != nil {
			d.logger.Warn("notification consumers failed to subscribe", slog.String("error", err.Error()))
		}
		if err := audit.NewEventConsumer(d.bus, d.auditSvc, d.logger).Subscribe(d.ctx); err != nil {
			d.logger.Warn("audit consumer failed to subscribe", slog.String("error", err.Error()))
		}
	}
	go notification.NewSLAMonitor(d.pool, d.notifSvc, time.Duration(d.cfg.SLAWarnHoursBefore)*time.Hour, d.logger).Run(d.ctx)
	go audit.NewIntegrityVerifier(d.auditSvc, d.metrics.AuditChainVerifyDuration, d.logger).Run(d.ctx)
	go audit.NewExportWorker(d.auditSvc, audit.TenantTxRunner(d.tenantTxRunner), d.cfg.StorageLocalBasePath, d.logger).Run(d.ctx)
	if getEnvBool("SETUP_SEED_SAMPLE_DATA", false) {
		go seedSampleData(d.ctx, d.pool, d.logger)
	}
}

func whoamiHandler(w http.ResponseWriter, r *http.Request) {
	uid, _ := httpx.UserID(r.Context())
	role, _ := httpx.Role(r.Context())
	httpx.RespondJSON(w, http.StatusOK, map[string]string{"userId": uid.String(), "role": role})
}