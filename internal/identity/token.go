package identity

// Package identity implements the bounded-context module for users, roles,
// permissions, and authentication (JWT + refresh-cookie + OAuth SSO)
// (blueprint §3.2, ADR-7).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Claims carried in every access token.
type Claims struct {
	TenantID string `json:"tenantId"`
	UserID   string `json:"userId"`
	Role     string `json:"role"`
	TokenVer int    `json:"tokenVersion"`
	jwt.RegisteredClaims
}

// TokenService issues and verifies RS256 access tokens (ADR-7). RS256 is
// chosen so a future extracted identity service can expose a JWKS endpoint.
type TokenService struct {
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	accessTTL  time.Duration
	issuer     string
}

// NewTokenService parses a PEM-encoded RSA private key.
func NewTokenService(privateKeyPEM string, accessTTL time.Duration) (*TokenService, error) {
	key, err := parseRSAPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("identity: parse private key: %w", err)
	}
	return &TokenService{
		privateKey: key,
		publicKey:  &key.PublicKey,
		accessTTL:  accessTTL,
		issuer:     "audita-api",
	}, nil
}

// Issue creates a signed access token for the user.
func (s *TokenService) Issue(tenantID, userID uuid.UUID, role string, tokenVersion int) (string, error) {
	now := time.Now()
	claims := Claims{
		TenantID: tenantID.String(),
		UserID:   userID.String(),
		Role:     role,
		TokenVer: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTTL)),
			ID:        uuid.NewString(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("identity: sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates a token, returning its claims.
func (s *TokenService) Verify(tokenStr string) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("identity: unexpected signing method %v", t.Header["alg"])
		}
		return s.publicKey, nil
	})
	if err != nil || !tok.Valid {
		return nil, errors.New("identity: invalid token")
	}
	claims, ok := tok.Claims.(*Claims)
	if !ok {
		return nil, errors.New("identity: invalid claims")
	}
	return claims, nil
}

// PublicKeyPEM returns the PEM-encoded public key (for JWKS exposure in a
// future service split, ADR-7).
func (s *TokenService) PublicKeyPEM() (string, error) {
	return encodeRSAPublicKeyPEM(s.publicKey)
}

// HashPassword hashes a plaintext password with bcrypt (default cost).
func HashPassword(plain string) (string, error) {
	if len(plain) < 8 {
		return "", errors.New("identity: password must be at least 8 characters")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("identity: hash password: %w", err)
	}
	return string(b), nil
}

// CheckPassword compares plaintext against a bcrypt hash.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NewOpaqueToken generates a cryptographically random opaque refresh/invite
// token and its SHA-256 hash for at-rest storage (plaintext is shown/emailed
// once; only the hash is persisted).
func NewOpaqueToken() (plaintext string, tokenHash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("identity: random token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(b)
	tokenHash = HashOpaqueToken(plaintext)
	return plaintext, tokenHash, nil
}

// NewKeyPairPEM generates a fresh RSA-2048 keypair, returning PEM strings —
// used by tooling (cmd/keygen) to bootstrap JWT_SECRET env values.
func NewKeyPairPEM() (privatePEM, publicPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("identity: generate key: %w", err)
	}
	priv, err := encodeRSAPrivateKeyPEM(key)
	if err != nil {
		return "", "", err
	}
	pub, err := encodeRSAPublicKeyPEM(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return priv, pub, nil
}

// LogTokenIssued emits a structured audit-adjacent log line (not an audit
// entry — that flows through the event bus) for token issuance events.
func LogTokenIssued(logger *slog.Logger, ctx context.Context, userID uuid.UUID) {
	logger.InfoContext(ctx, "token_issued", slog.String("userId", userID.String()))
}
