package idempotency

// Package idempotency implements the shared Idempotency-Key mechanism for
// mutating endpoints (blueprint §4.3), carried over from v1.

import (
	"context"
	"errors"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record represents a stored idempotency result for a prior request.
type Record struct {
	Key         string
	Operation   string
	ResourceID  string
	Response    []byte
	StatusCode  int
}

// Store persists idempotency keys per user + operation, carried over from v1's
// IdempotencyKeyEntity behaviour.
type Store struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// NewStore builds a Store; keys older than ttl are pruned lazily.
func NewStore(pool *pgxpool.Pool, ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Store{pool: pool, ttl: ttl}
}

// ErrConflict indicates the same key was reused with a different operation.
var ErrConflict = httpx.NewProblem(409, httpx.CodeIdempotencyConflict,
	"idempotency key was already used for a different operation")

// ErrNotFound indicates no prior result exists for this key.
var ErrNotFound = errors.New("idempotency: no prior result")

// Lookup returns the cached response for (userID, operation, key) if present.
func (s *Store) Lookup(ctx context.Context, userID uuid.UUID, operation, key string) (*Record, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT operation, resource_id, response_body, response_status
		FROM idempotency_keys
		WHERE user_id = $1 AND operation = $2 AND idempotency_key = $3 AND expires_at > now()`,
		userID, operation, key)
	var op, resourceID string
	var body []byte
	var status int
	if err := row.Scan(&op, &resourceID, &body, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if op != operation {
		return nil, ErrConflict
	}
	return &Record{Key: key, Operation: op, ResourceID: resourceID, Response: body, StatusCode: status}, nil
}

// Save records the response for a key, upserting so a retry after a crash
// during the original mutation can still persist the result.
func (s *Store) Save(ctx context.Context, userID uuid.UUID, operation, key, resourceID string, status int, body []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys
			(user_id, operation, idempotency_key, resource_id, response_body, response_status, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7), now())
		ON CONFLICT (user_id, operation, idempotency_key)
		DO UPDATE SET response_body = EXCLUDED.response_body,
		              response_status = EXCLUDED.response_status,
		              resource_id = EXCLUDED.resource_id`,
		userID, operation, key, resourceID, body, status, s.ttl.Seconds())
	return err
}

// Prune removes expired keys; safe to run from a background job.
func (s *Store) Prune(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	return err
}
