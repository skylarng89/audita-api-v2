package identity

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RefreshCookieName is the httpOnly cookie carrying the rotating refresh
// token (ADR-7).
const RefreshCookieName = "audita_refresh"

// RefreshCookiePath scopes the cookie to auth endpoints only.
const RefreshCookiePath = "/api/v1/auth"

// SetRefreshCookie writes the refresh cookie. The Secure flag is always
// enabled: refresh tokens are high-value credentials and are only delivered
// over HTTPS in every supported deployment topology (ADR-7).
func SetRefreshCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    token,
		Path:     RefreshCookiePath,
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearRefreshCookie expires the refresh cookie on logout.
func ClearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    "",
		Path:     RefreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// Authenticator verifies access tokens and supplies user identity to handlers.
type Authenticator struct {
	tokens *TokenService
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(tokens *TokenService) *Authenticator { return &Authenticator{tokens: tokens} }

// Middleware validates the Bearer JWT, checks tenant claim against the
// resolved tenant, and injects user identity into ctx.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "missing bearer token"))
			return
		}
		claims, err := a.tokens.Verify(raw)
		if err != nil {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid or expired token"))
			return
		}
		tenantID, hasTenant := httpx.TenantID(r.Context())
		if !hasTenant || tenantID.String() != claims.TenantID {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "token tenant mismatch"))
			return
		}
		userID, err := uuid.Parse(claims.UserID)
		if err != nil {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid subject claim"))
			return
		}
		ctx := httpx.WithUserID(r.Context(), userID)
		ctx = httpx.WithRole(ctx, claims.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole enforces that the authenticated user's role is one of allowed.
func RequireRole(allowed ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, ok := httpx.Role(r.Context())
			if !ok {
				httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "not authenticated"))
				return
			}
			for _, a := range allowed {
				if strings.EqualFold(role, a) {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusForbidden, httpx.CodeForbidden, "insufficient role"))
		})
	}
}

// TenantTxRunner runs a function inside a tenant-scoped RLS transaction
// resolved from the request context. Provided by the composition root (cmd)
// so identity does not import platform directly (ADR-1 boundary).
type TenantTxRunner func(ctx context.Context, fn func(tx pgx.Tx) error) error

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}
