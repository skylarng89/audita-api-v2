package tenancy

import (
	"net/http"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Handler exposes tenancy REST endpoints (platform-scope).
type Handler struct {
	svc *Service
}

// NewHandler builds a Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes registers platform tenant endpoints on a subrouter. The caller is
// responsible for wrapping this with super-admin auth middleware.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/tenants", h.list)
	r.Post("/tenants", h.create)
	r.Get("/tenants/{id}", h.get)
	r.Post("/tenants/{id}/status", h.setStatus)
}

type createTenantReq struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Subdomain string `json:"subdomain"`
}

// create godoc
// @Summary Create a tenant
// @Tags platform
// @Accept json
// @Produce json
// @Param body body createTenantReq true "Tenant to create"
// @Success 201 {object} Tenant
// @Router /api/platform/v1/tenants [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createTenantReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	t, err := h.svc.Create(r.Context(), req.Name, req.Slug, req.Subdomain)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, t)
}

// list godoc
// @Summary List tenants
// @Tags platform
// @Produce json
// @Success 200 {array} Tenant
// @Router /api/platform/v1/tenants [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ts, err := h.svc.List(r.Context())
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, ts)
}

// get godoc
// @Summary Get tenant by ID
// @Tags platform
// @Produce json
// @Success 200 {object} Tenant
// @Router /api/platform/v1/tenants/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r, "id")
	if !ok {
		return
	}
	t, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, t)
}

type setStatusReq struct {
	Status string `json:"status"`
}

// setStatus godoc
// @Summary Update tenant status
// @Tags platform
// @Accept json
// @Success 204
// @Router /api/platform/v1/tenants/{id}/status [post]
func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r, "id")
	if !ok {
		return
	}
	var req setStatusReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if err := h.svc.SetStatus(r.Context(), id, req.Status); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
