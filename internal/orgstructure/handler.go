package orgstructure

import (
	"net/http"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes org-structure admin endpoints.
type Handler struct {
	svc *Service
}

// NewHandler builds an orgstructure Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes registers admin endpoints (admin-gated by the caller).
func (h *Handler) Routes(r chi.Router) {
	r.Route("/groups", func(g chi.Router) {
		g.Get("/", h.listGroups)
		g.Post("/", h.createGroup)
		g.Route("/{id}", func(one chi.Router) {
			one.Put("/", h.updateGroup)
			one.Delete("/", h.deleteGroup)
			one.Get("/members", h.listMembers)
			one.Post("/members", h.addMember)
			one.Delete("/members/{userId}", h.removeMember)
		})
	})
	r.Route("/settings/departments", func(d chi.Router) {
		d.Get("/", h.listDepartments)
		d.Post("/", h.createDepartment)
		d.Delete("/{id}", h.deleteDepartment)
	})
	r.Route("/admin/custom-fields", func(cf chi.Router) {
		cf.Get("/", h.listCustomFields)
		cf.Post("/", h.createCustomField)
		cf.Delete("/{id}", h.deleteCustomField)
	})
}

// listGroups godoc
// @Summary List groups (with member counts)
// @Success 200 {array} Group
// @Router /api/v1/groups [get]
func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListGroups(r.Context())
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*Group{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// createGroup godoc
// @Summary Create a group
// @Accept json
// @Success 201 {object} Group
// @Router /api/v1/groups [post]
func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		DisplayOrder int    `json:"displayOrder"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	g, err := h.svc.CreateGroup(r.Context(), req.Name, req.Description, req.DisplayOrder)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, g)
}

// updateGroup godoc
// @Summary Update a group
// @Success 204
// @Router /api/v1/groups/{id} [put]
func (h *Handler) updateGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		IsActive     *bool  `json:"isActive"`
		DisplayOrder int    `json:"displayOrder"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	if err := h.svc.UpdateGroup(r.Context(), id, req.Name, req.Description, isActive, req.DisplayOrder); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteGroup godoc
// @Summary Delete a group
// @Success 204
// @Router /api/v1/groups/{id} [delete]
func (h *Handler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteGroup(r.Context(), id); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListGroupMembers(r.Context(), id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*GroupMember{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// addMember godoc
// @Summary Add a member to a group
// @Accept json
// @Success 201 {object} GroupMember
// @Router /api/v1/groups/{id}/members [post]
func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	groupID, ok := parseID(w, r)
	if !ok {
		return
	}
	var req struct {
		UserID uuid.UUID `json:"userId"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	m, err := h.svc.AddGroupMember(r.Context(), groupID, req.UserID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, m)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	groupID, ok := parseID(w, r)
	if !ok {
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid userId"))
		return
	}
	if err := h.svc.RemoveGroupMember(r.Context(), groupID, userID); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listDepartments godoc
// @Summary List departments
// @Success 200 {array} Department
// @Router /api/v1/settings/departments [get]
func (h *Handler) listDepartments(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListDepartments(r.Context())
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*Department{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// createDepartment godoc
// @Summary Create a department
// @Accept json
// @Success 201 {object} Department
// @Router /api/v1/settings/departments [post]
func (h *Handler) createDepartment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Code         string `json:"code"`
		DisplayOrder int    `json:"displayOrder"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	d, err := h.svc.CreateDepartment(r.Context(), req.Name, req.Code, req.DisplayOrder)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, d)
}

// deleteDepartment godoc
// @Summary Delete a department
// @Success 204
// @Router /api/v1/settings/departments/{id} [delete]
func (h *Handler) deleteDepartment(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteDepartment(r.Context(), id); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listCustomFields(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListCustomFields(r.Context())
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*CustomFieldDefinition{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// createCustomField godoc
// @Summary Create a custom-field definition
// @Accept json
// @Success 201 {object} CustomFieldDefinition
// @Router /api/v1/admin/custom-fields [post]
func (h *Handler) createCustomField(w http.ResponseWriter, r *http.Request) {
	var def CustomFieldDefinition
	if err := httpx.DecodeJSON(r, &def); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	created, err := h.svc.CreateCustomField(r.Context(), &def)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, created)
}

// deleteCustomField godoc
// @Summary Delete a custom-field definition
// @Success 204
// @Router /api/v1/admin/custom-fields/{id} [delete]
func (h *Handler) deleteCustomField(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteCustomField(r.Context(), id); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid id"))
		return uuid.Nil, false
	}
	return id, true
}
