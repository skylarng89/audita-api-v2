// Command migrate runs database migrations as a standalone one-shot job
// (blueprint Sprint 0, Task 0.4 / ADR-9 compose `migrate` service).
package main

import (
	"context"
	"flag"
	"time"

	"github.com/audita/audita-api/internal/platform/db"
	"github.com/audita/audita-api/internal/platform/runtime"
)

func main() {
	var (
		databaseURL    = flag.String("url", envOr("DATABASE_URL", ""), "Postgres connection URL")
		migrationsPath = flag.String("path", envOr("MIGRATIONS_PATH", "./internal/migrations"), "path to migrations directory")
		waitTimeout    = flag.Duration("wait", 60*time.Second, "how long to wait for the DB to become reachable")
	)
	flag.Parse()
	logger := runtime.NewLogger("info")

	if *databaseURL == "" {
		runtime.Fatalf(logger, "migrate: DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *waitTimeout+30*time.Second)
	defer cancel()

	if err := db.WaitForDB(ctx, *databaseURL, *waitTimeout, logger); err != nil {
		runtime.Fatalf(logger, "database not reachable: %v", err)
	}
	if err := db.MigrateUp(*databaseURL, *migrationsPath); err != nil {
		platform.Fatalf(logger, "migrate: %v", err)
	}
	logger.Info("migrations applied")
}

func envOr(key, def string) string {
	if v, ok := lookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
