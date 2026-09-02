package identity

import (
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// Handler exposes identity REST endpoints.
type Handler struct {
	svc          *Service
	tx           TenantTxRunner
	inviteExpiry time.Duration
}

// NewHandler builds an identity Handler.
func NewHandler(svc *Service, tx TenantTxRunner, inviteExpiry time.Duration) *Handler {
	return &Handler{svc: svc, tx: tx, inviteExpiry: inviteExpiry}
}

// Routes registers auth endpoints under the tenant-scoped router. The
// tenant-resolution middleware must be applied OUTSIDE this router; auth
// endpoints themselves do not require a prior bearer token.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)
	r.Get("/auth/session", h.session)
	r.Post("/auth/forgot-password", h.forgotPassword)
	r.Post("/auth/reset-password", h.resetPassword)
	r.Post("/auth/accept-invite", h.acceptInvite)
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// login godoc
// @Summary Login with email/password
// @Tags auth
// @Accept json
// @Produce json
// @Param body body loginReq true "Credentials"
// @Success 200 {object} LoginResult
// @Router /api/v1/auth/login [post]
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := httpx.TenantID(r.Context())
	if !ok {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "TENANT_REQUIRED", "missing tenant context"))
		return
	}
	var req loginReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	res, err := h.svc.Login(r.Context(), tenantID, req.Email, req.Password)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	SetRefreshCookie(w, res.RefreshToken, res.RefreshExpiresAt)
	httpx.RespondJSON(w, http.StatusOK, res)
}

// refresh godoc
// @Summary Rotate refresh token and re-issue access token
// @Tags auth
// @Success 200 {object} LoginResult
// @Router /api/v1/auth/refresh [post]
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := httpx.TenantID(r.Context())
	if !ok {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "TENANT_REQUIRED", "missing tenant context"))
		return
	}
	c, err := r.Cookie(RefreshCookieName)
	if err != nil || c.Value == "" {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "missing refresh cookie"))
		return
	}
	res, err := h.svc.Refresh(r.Context(), tenantID, c.Value)
	if err != nil {
		ClearRefreshCookie(w)
		httpx.RespondProblem(w, r, err)
		return
	}
	SetRefreshCookie(w, res.RefreshToken, res.RefreshExpiresAt)
	httpx.RespondJSON(w, http.StatusOK, res)
}

// logout godoc
// @Summary Revoke refresh token and clear cookie
// @Tags auth
// @Success 204
// @Router /api/v1/auth/logout [post]
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(RefreshCookieName); err == nil && c.Value != "" {
		_ = h.svc.Logout(r.Context(), c.Value)
	}
	ClearRefreshCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// session godoc
// @Summary Restore the current session from the access token
// @Tags auth
// @Produce json
// @Success 200 {object} User
// @Router /api/v1/auth/session [get]
func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "not authenticated"))
		return
	}
	var u *User
	err := h.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		u, err = GetUserByIDTx(r.Context(), tx, userID)
		return err
	})
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, u)
}

type forgotReq struct {
	Email string `json:"email"`
}

// forgotPassword godoc
// @Summary Request a password-reset email
// @Tags auth
// @Success 202
// @Router /api/v1/auth/forgot-password [post]
func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := httpx.TenantID(r.Context())
	var req forgotReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	// Always returns 202 to avoid account enumeration; email dispatch is
	// handled by the notification/email pipeline.
	_, _ = h.svc.RequestPasswordReset(r.Context(), req.Email, h.inviteExpiry)
	w.WriteHeader(http.StatusAccepted)
}

type resetReq struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

// resetPassword godoc
// @Summary Consume a reset token and set a new password
// @Tags auth
// @Success 204
// @Router /api/v1/auth/reset-password [post]
func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := httpx.TenantID(r.Context())
	var req resetReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if err := h.svc.ResetPassword(r.Context(), req.Token, req.NewPassword); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type acceptInviteReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// acceptInvite godoc
// @Summary Activate a pending user via an invite token
// @Tags auth
// @Success 200 {object} User
// @Router /api/v1/auth/accept-invite [post]
func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := httpx.TenantID(r.Context())
	var req acceptInviteReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	u, err := h.svc.AcceptInvite(r.Context(), req.Token, req.Password)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, u)
}
