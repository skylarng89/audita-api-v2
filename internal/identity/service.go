package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UserStatus values.
const (
	UserStatusPending     = "pending"
	UserStatusActive      = "active"
	UserStatusDeactivated = "deactivated"
)

// User is the identity module's user aggregate.
type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	FullName     string    `json:"fullName"`
	RoleID       *uuid.UUID `json:"roleId,omitempty"`
	RoleName     string    `json:"roleName"`
	Status       string    `json:"status"`
	TokenVersion int       `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Service implements identity use-cases: login, refresh, logout, invites,
// password reset (FR-7, Sprint 1 tasks 1.4/1.5). All tenant-scoped DB access
// flows through the TenantTxRunner so RLS is enforced centrally (ADR-4).
type Service struct {
	tx         TenantTxRunner
	tokens     *TokenService
	refreshTTL time.Duration
}

// NewService builds an identity Service.
func NewService(tx TenantTxRunner, tokens *TokenService, refreshTTL time.Duration) *Service {
	return &Service{tx: tx, tokens: tokens, refreshTTL: refreshTTL}
}

// LoginResult carries issued tokens plus the user for session restore.
type LoginResult struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"-"` // never serialised; set as cookie
	User             *User  `json:"user"`
	RefreshExpiresAt time.Time `json:"-"`
}

// ErrInvalidCredentials is returned for bad email/password.
var ErrInvalidCredentials = httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid credentials")

// ErrAccountDeactivated is returned when a deactivated user attempts login.
var ErrAccountDeactivated = httpx.NewProblem(http.StatusForbidden, httpx.CodeForbidden, "account is deactivated")

// Login authenticates a user by email/password, issuing an access token and
// a new rotating refresh token.
func (s *Service) Login(ctx context.Context, tenantID uuid.UUID, email, password string) (*LoginResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var (
		u    *User
		hash string
	)
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		u, hash, err = GetUserByEmailTx(ctx, tx, email)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("identity: login: %w", err)
	}
	switch {
	case u.Status == UserStatusDeactivated:
		return nil, ErrAccountDeactivated
	case u.Status == UserStatusPending:
		return nil, httpx.NewProblem(http.StatusForbidden, httpx.CodeForbidden, "account invitation not yet accepted")
	}
	if !CheckPassword(hash, password) {
		return nil, ErrInvalidCredentials
	}
	return s.issueTokens(ctx, tenantID, u)
}

// Refresh validates a presented refresh token, rotates it, and issues a new
// access token. Rotation-on-use with revocation (ADR-7).
func (s *Service) Refresh(ctx context.Context, tenantID uuid.UUID, presentedToken string) (*LoginResult, error) {
	tokenHash := HashOpaqueToken(presentedToken)
	var u *User
	err := s.tx(ctx, func(tx pgx.Tx) error {
		user, err := LookupUserByRefreshTokenTx(ctx, tx, tokenHash)
		if err != nil {
			return err
		}
		u = user
		return RevokeRefreshTokenTx(ctx, tx, tokenHash)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid refresh token")
		}
		return nil, fmt.Errorf("identity: refresh: %w", err)
	}
	if u.Status != UserStatusActive {
		return nil, ErrAccountDeactivated
	}
	return s.issueTokens(ctx, tenantID, u)
}

// Logout revokes the presented refresh token (idempotent).
func (s *Service) Logout(ctx context.Context, presentedToken string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		return RevokeRefreshTokenTx(ctx, tx, HashOpaqueToken(presentedToken))
	})
}

// InviteUser creates a pending user and returns the plaintext invite token
// (emailed by the caller via platform/email).
func (s *Service) InviteUser(ctx context.Context, invitedBy uuid.UUID, email, fullName string, roleID *uuid.UUID, expiry time.Duration) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || fullName == "" {
		return "", httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "email and fullName are required")
	}
	plaintext, tokenHash, err := NewOpaqueToken()
	if err != nil {
		return "", err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		_, err := CreatePendingUserTx(ctx, tx, inviteUserParams{
			Email:           email,
			FullName:        fullName,
			RoleID:          roleID,
			InvitedBy:       invitedBy,
			InviteTokenHash: tokenHash,
			Expiry:          expiry,
		})
		return err
	})
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// AcceptInvite activates a pending user with a chosen password.
func (s *Service) AcceptInvite(ctx context.Context, token, password string) (*User, error) {
	if len(password) < 8 {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "password must be at least 8 characters")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	var u *User
	err = s.tx(ctx, func(tx pgx.Tx) error {
		user, err := AcceptInviteTx(ctx, tx, HashOpaqueToken(token), hash)
		if err != nil {
			return err
		}
		u = user
		return nil
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// RequestPasswordReset generates a reset token (returns "" for unknown
// emails to avoid enumeration).
func (s *Service) RequestPasswordReset(ctx context.Context, email string, expiry time.Duration) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	plaintext, tokenHash, err := NewOpaqueToken()
	if err != nil {
		return "", err
	}
	var found bool
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		found, err = CreatePasswordResetTokenTx(ctx, tx, email, tokenHash, expiry)
		return err
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return plaintext, nil
}

// ResetPassword consumes a reset token and sets a new password.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if len(newPassword) < 8 {
		return httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "password must be at least 8 characters")
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		return ConsumePasswordResetTokenTx(ctx, tx, HashOpaqueToken(token), hash)
	})
}

// ListUsers returns all tenant users.
func (s *Service) ListUsers(ctx context.Context) ([]*User, error) {
	var out []*User
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = ListUsersTx(ctx, tx)
		return err
	})
	return out, err
}

func (s *Service) issueTokens(ctx context.Context, tenantID uuid.UUID, u *User) (*LoginResult, error) {
	access, err := s.tokens.Issue(tenantID, u.ID, u.RoleName, u.TokenVersion)
	if err != nil {
		return nil, err
	}
	plaintext, tokenHash, err := NewOpaqueToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(s.refreshTTL)
	err = s.tx(ctx, func(tx pgx.Tx) error {
		return StoreRefreshTokenTx(ctx, tx, u.ID, tokenHash, expires)
	})
	if err != nil {
		return nil, err
	}
	return &LoginResult{
		AccessToken:      access,
		RefreshToken:     plaintext,
		User:             u,
		RefreshExpiresAt: expires,
	}, nil
}
