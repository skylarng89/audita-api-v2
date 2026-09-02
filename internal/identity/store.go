package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// inviteUserParams bundles CreatePendingUserTx inputs to keep the function
// under the parameter-count lint limit.
type inviteUserParams struct {
	Email           string
	FullName        string
	RoleID          *uuid.UUID
	InvitedBy       uuid.UUID
	InviteTokenHash string
	Expiry          time.Duration
}

// This file contains all identity-module SQL, expressed as functions over an
// active pgx.Tx. Every caller MUST invoke these inside a tenant-scoped
// transaction (TenantTxRunner) so Postgres RLS scopes rows to the current
// tenant (blueprint ADR-4). No function here touches the raw pool.

const userSelectCols = `u.id, u.email, u.full_name, u.role_id, COALESCE(r.name, ''), u.status, u.token_version, u.created_at`

func scanUserRow(row pgx.Row) (*User, error) {
	var u User
	var roleID *uuid.UUID
	var hash *string
	if err := row.Scan(&u.ID, &u.Email, &u.FullName, &roleID, &u.RoleName, &u.Status, &u.TokenVersion, &u.CreatedAt, &hash); err != nil {
		return nil, err
	}
	u.RoleID = roleID
	return &u, nil
}

// GetUserByEmailTx fetches a user (any status) by email with password hash.
func GetUserByEmailTx(ctx context.Context, tx pgx.Tx, email string) (*User, string, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+userSelectCols+`, u.password_hash
		FROM users u LEFT JOIN roles r ON r.id = u.role_id
		WHERE LOWER(u.email) = LOWER($1)`, email)
	var u User
	var roleID *uuid.UUID
	var hash *string
	if err := row.Scan(&u.ID, &u.Email, &u.FullName, &roleID, &u.RoleName, &u.Status, &u.TokenVersion, &u.CreatedAt, &hash); err != nil {
		return nil, "", err
	}
	u.RoleID = roleID
	outHash := ""
	if hash != nil {
		outHash = *hash
	}
	return &u, outHash, nil
}

// GetUserByIDTx fetches a user by ID (no password hash).
func GetUserByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*User, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+userSelectCols+`, NULL
		FROM users u LEFT JOIN roles r ON r.id = u.role_id
		WHERE u.id = $1`, id)
	return scanUserRow(row)
}

// ListUsersTx lists tenant users, newest first.
func ListUsersTx(ctx context.Context, tx pgx.Tx) ([]*User, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+userSelectCols+`, NULL
		FROM users u LEFT JOIN roles r ON r.id = u.role_id
		ORDER BY u.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		var u User
		var roleID *uuid.UUID
		var hash *string
		if err := rows.Scan(&u.ID, &u.Email, &u.FullName, &roleID, &u.RoleName, &u.Status, &u.TokenVersion, &u.CreatedAt, &hash); err != nil {
			return nil, err
		}
		u.RoleID = roleID
		out = append(out, &u)
	}
	return out, rows.Err()
}

// CreatePendingUserTx inserts a pending user plus invite token.
func CreatePendingUserTx(ctx context.Context, tx pgx.Tx, p inviteUserParams) (*User, error) {
	id := uuid.New()
	tag, err := tx.Exec(ctx, `
		INSERT INTO users (id, email, full_name, role_id, status, invited_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email) DO NOTHING`,
		id, p.Email, p.FullName, p.RoleID, UserStatusPending, p.InvitedBy)
	if err != nil {
		return nil, fmt.Errorf("identity: create pending user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, httpx.NewProblem(http.StatusConflict, httpx.CodeConflict, "a user with this email already exists")
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO invite_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, uuid.New(), id, p.InviteTokenHash, time.Now().Add(p.Expiry))
	if err != nil {
		return nil, fmt.Errorf("identity: create invite token: %w", err)
	}
	u, err := GetUserByEmailTx(ctx, tx, email)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// AcceptInviteTx consumes an invite token and activates the user with a
// bcrypt password hash.
func AcceptInviteTx(ctx context.Context, tx pgx.Tx, tokenHash, passwordHash string) (*User, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx, `
		UPDATE users u SET status = $2, password_hash = $3, updated_at = now()
		FROM invite_tokens it
		WHERE it.user_id = u.id
		  AND it.token_hash = $1
		  AND it.used_at IS NULL
		  AND it.expires_at > now()
		RETURNING u.id`,
		tokenHash, UserStatusActive, passwordHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.NewProblem(http.StatusBadRequest, "INVALID_INVITE_TOKEN", "invite token is invalid, already used, or expired")
	}
	if err != nil {
		return nil, fmt.Errorf("identity: accept invite: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE invite_tokens SET used_at = now() WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return nil, err
	}
	return GetUserByIDTx(ctx, tx, userID)
}

// CreatePasswordResetTokenTx stores a reset token for an active user.
// Returns false when the email is unknown/inactive (do not reveal).
func CreatePasswordResetTokenTx(ctx context.Context, tx pgx.Tx, email, tokenHash string, expiry time.Duration) (bool, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM users WHERE LOWER(email) = LOWER($1) AND status = $2`, email, UserStatusActive).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, uuid.New(), userID, tokenHash, time.Now().Add(expiry))
	return true, err
}

// ConsumePasswordResetTokenTx applies a password reset and bumps the token
// version, invalidating outstanding access tokens.
func ConsumePasswordResetTokenTx(ctx context.Context, tx pgx.Tx, tokenHash, passwordHash string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE users u SET password_hash = $2, token_version = u.token_version + 1, updated_at = now()
		FROM password_reset_tokens prt
		WHERE prt.user_id = u.id
		  AND prt.token_hash = $1
		  AND prt.used_at IS NULL
		  AND prt.expires_at > now()`,
		tokenHash, passwordHash)
	if err != nil {
		return fmt.Errorf("identity: reset password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return httpx.NewProblem(http.StatusBadRequest, "INVALID_RESET_TOKEN", "reset token is invalid, already used, or expired")
	}
	_, err = tx.Exec(ctx, `UPDATE password_reset_tokens SET used_at = now() WHERE token_hash = $1`, tokenHash)
	return err
}

// StoreRefreshTokenTx persists a hashed refresh token.
func StoreRefreshTokenTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, tokenHash string, expires time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, uuid.New(), userID, tokenHash, expires)
	return err
}

// LookupUserByRefreshTokenTx resolves a valid refresh token hash to a user.
func LookupUserByRefreshTokenTx(ctx context.Context, tx pgx.Tx, tokenHash string) (*User, error) {
	row := tx.QueryRow(ctx, `
		SELECT `+userSelectCols+`, NULL
		FROM refresh_tokens rt
		JOIN users u ON u.id = rt.user_id
		LEFT JOIN roles r ON r.id = u.role_id
		WHERE rt.token_hash = $1 AND rt.revoked_at IS NULL AND rt.expires_at > now()`, tokenHash)
	return scanUserRow(row)
}

// RevokeRefreshTokenTx revokes a token (idempotent).
func RevokeRefreshTokenTx(ctx context.Context, tx pgx.Tx, tokenHash string) error {
	_, err := tx.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash)
	return err
}
