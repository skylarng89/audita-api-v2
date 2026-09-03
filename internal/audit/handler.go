package audit

import (
	"net/http"
	"strconv"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes audit trail REST endpoints.
type Handler struct {
	svc *Service
}

// NewHandler builds an audit Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes registers audit endpoints (admin-gated by the caller).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/audit-trail", h.list)
	r.Post("/audit-trail/verify", h.verify)
	r.Post("/audit-trail/export", h.requestExport)
	r.Get("/audit-trail/export/{exportId}", h.exportStatus)
}

// list godoc
// @Summary List audit entries (filterable)
// @Produce json
// @Param actorId query string false "actor UUID"
// @Param entityType query string false "entity type"
// @Param entityId query string false "entity UUID"
// @Param limit query int false "max results (default 100, max 500)"
// @Success 200 {array} Entry
// @Router /api/v1/audit-trail [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	f := ListFilter{}
	if raw := r.URL.Query().Get("actorId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid actorId"))
			return
		}
		f.ActorID = &id
	}
	f.EntityType = r.URL.Query().Get("entityType")
	if raw := r.URL.Query().Get("entityId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid entityId"))
			return
		}
		f.EntityID = &id
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid limit"))
			return
		}
		f.Limit = n
	}
	list, err := h.svc.List(r.Context(), f)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*Entry{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// verify godoc
// @Summary Verify the audit hash chain (admin)
// @Success 200 {object} VerifyResult
// @Router /api/v1/audit-trail/verify [post]
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.VerifyChain(r.Context())
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, res)
}

// requestExport godoc
// @Summary Request an async CSV export
// @Accept json
// @Success 202 {object} ExportRequest
// @Router /api/v1/audit-trail/export [post]
func (h *Handler) requestExport(w http.ResponseWriter, r *http.Request) {
	actorID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "not authenticated"))
		return
	}
	var filters map[string]any
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &filters); err != nil {
			httpx.RespondProblem(w, r, err)
			return
		}
	}
	req, err := h.svc.CreateExportRequest(r.Context(), actorID, filters)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusAccepted, req)
}

// exportStatus godoc
// @Summary Poll an export request's status
// @Success 200 {object} ExportRequest
// @Router /api/v1/audit-trail/export/{exportId} [get]
func (h *Handler) exportStatus(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "exportId")
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid exportId"))
		return
	}
	req, err := h.svc.GetExportRequest(r.Context(), id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, req)
}
