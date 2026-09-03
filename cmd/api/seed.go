package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/audita/audita-api/internal/sampledata"
)

// seedSampleData seeds the first tenant with demo data (Sprint 5, Task 5.5).
// Gated by SETUP_SEED_SAMPLE_DATA=true; failures are logged, never fatal.
func seedSampleData(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	var tenantID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM tenants ORDER BY created_at LIMIT 1`).Scan(&tenantID); err != nil {
		logger.Warn("sample data seeding skipped: no tenant found", slog.String("error", err.Error()))
		return
	}
	seeder := sampledata.NewSeeder(pool, logger)
	if err := seeder.Seed(ctx, tenantID); err != nil {
		logger.Warn("sample data seeding failed", slog.String("error", err.Error()))
	}
}