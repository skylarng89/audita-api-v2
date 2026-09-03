package changerequest

import (
	"net/http"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the CR REST surface under /api/v1/change-requests.
type Handler struct {
	svc *Service
}

// NewHandler builds a CR Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes registers CR endpoints on an authenticated router.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/change-requests", func(cr chi.Router) {
		cr.Get("/", h.list)
		cr.Post("/", h.create)
		cr.Route("/{id}", func(one chi.Router) {
			one.Get("/", h.get)
			one.Patch("/", h.update)
			one.Post("/submit", h.submit)
			one.Post("/complete", h.complete)
			one.Post("/cancel", h.cancel)

			one.Post("/approvers", h.addApprover)
			one.Get("/approvers", h.listApprovers)
			one.Delete("/approvers/{approverId}", h.removeApprover)
			one.Post("/approvers/{approverId}/promote", h.promote)
			one.Post("/approvers/{approverId}/demote", h.demote)
			one.Post("/approvers/{approverId}/decision", h.decide)
			one.Post("/approver-groups", h.addApproverGroup)
			one.Get("/approver-groups", h.listApproverGroups)
			one.Post("/approver-groups/{groupId}/decision", h.groupDecide)

			one.Post("/watchers", h.addWatcher)
			one.Get("/watchers", h.listWatchers)
			one.Delete("/watchers/{watcherId}", h.removeWatcher)
			one.Post("/watcher-groups", h.addWatcherGroup)
			one.Get("/watcher-groups", h.listWatcherGroups)

			one.Get("/approver-candidates", h.approverCandidates)
		})
	})
}

// create godoc
// @Summary Create a change request draft
// @Tags change-requests
// @Accept json
// @Produce json
// @Success 201 {object} ChangeRequest
// @Router /api/v1/change-requests [post]
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	var req CreateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	cr, err := h.svc.Create(r.Context(), actorID, req)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, cr)
}

// list godoc
// @Summary List change requests
// @Produce json
// @Param status query string false "filter by status"
// @Success 200 {array} ChangeRequest
// @Router /api/v1/change-requests [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// get godoc
// @Summary Get a change request
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id} [get]
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	cr, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

// update godoc
// @Summary Update a change request (draft: full; submitted: whitelist)
// @Accept json
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id} [patch]
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	var req CreateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	cr, err := h.svc.Update(r.Context(), actorID, id, req)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

// submit godoc
// @Summary Submit a draft for approval
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id}/submit [post]
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	cr, err := h.svc.Submit(r.Context(), actorID, id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

// complete godoc
// @Summary Mark an approved change request complete
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id}/complete [post]
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	cr, err := h.svc.Complete(r.Context(), actorID, id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

// cancel godoc
// @Summary Cancel a draft or pending change request
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id}/cancel [post]
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	cr, err := h.svc.Cancel(r.Context(), actorID, id)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

type decisionReq struct {
	ApproverID      uuid.UUID `json:"approverId"`
	Approve         bool      `json:"approve"`
	RejectionReason string    `json:"rejectionReason"`
	DecisionComment string    `json:"decisionComment"`
}

// decide godoc
// @Summary Approve or reject as a direct approver
// @Accept json
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id}/approvers/{approverId}/decision [post]
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	approverID, ok := parseUUIDParam(w, r, "approverId")
	if !ok {
		return
	}
	var req decisionReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	cr, err := h.svc.Decide(r.Context(), actorID, crID, approverID, req.Approve, req.RejectionReason, req.DecisionComment)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

type groupDecisionReq struct {
	GroupID         uuid.UUID `json:"groupId"`
	Approve         bool      `json:"approve"`
	RejectionReason string    `json:"rejectionReason"`
}

// groupDecide godoc
// @Summary Approve or reject as a member of an approver group
// @Accept json
// @Success 200 {object} ChangeRequest
// @Router /api/v1/change-requests/{id}/approver-groups/{groupId}/decision [post]
func (h *Handler) groupDecide(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	groupID, ok := parseUUIDParam(w, r, "groupId")
	if !ok {
		return
	}
	var req groupDecisionReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	cr, err := h.svc.GroupApprove(r.Context(), actorID, crID, groupID, req.Approve, req.RejectionReason)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, cr)
}

type addApproverReq struct {
	UserID uuid.UUID `json:"userId"`
}

// addApprover godoc
// @Summary Add a direct approver
// @Accept json
// @Success 201 {object} Approver
// @Router /api/v1/change-requests/{id}/approvers [post]
func (h *Handler) addApprover(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	var req addApproverReq
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	ap, err := h.svc.AddApprover(r.Context(), actorID, crID, req.UserID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, ap)
}

// addApproverGroup godoc
// @Summary Add an approver group
// @Accept json
// @Success 201 {object} ApproverGroup
// @Router /api/v1/change-requests/{id}/approver-groups [post]
func (h *Handler) addApproverGroup(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	var req struct {
		GroupID uuid.UUID `json:"groupId"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	ag, err := h.svc.AddApproverGroup(r.Context(), actorID, crID, req.GroupID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, ag)
}

// addWatcher godoc
// @Summary Add a watcher
// @Accept json
// @Success 201 {object} Watcher
// @Router /api/v1/change-requests/{id}/watchers [post]
func (h *Handler) addWatcher(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
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
	wt, err := h.svc.AddWatcher(r.Context(), actorID, crID, req.UserID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, wt)
}

// addWatcherGroup godoc
// @Summary Add a watcher group
// @Accept json
// @Success 201 {object} WatcherGroup
// @Router /api/v1/change-requests/{id}/watcher-groups [post]
func (h *Handler) addWatcherGroup(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	var req struct {
		GroupID uuid.UUID `json:"groupId"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	wg, err := h.svc.AddWatcherGroup(r.Context(), actorID, crID, req.GroupID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, wg)
}

func (h *Handler) listApprovers(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListApprovers(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

func (h *Handler) listApproverGroups(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListApproversGroups(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

func (h *Handler) listWatchers(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListWatchers(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

func (h *Handler) listWatcherGroups(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListWatcherGroups(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

func (h *Handler) removeApprover(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	approverID, ok := parseUUIDParam(w, r, "approverId")
	if !ok {
		return
	}
	if err := h.svc.RemoveApprover(r.Context(), crID, approverID); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeWatcher(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	watcherID, ok := parseUUIDParam(w, r, "watcherId")
	if !ok {
		return
	}
	if err := h.svc.RemoveWatcher(r.Context(), crID, watcherID); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// promote godoc
// @Summary Promote a watcher to approver
// @Success 201 {object} Approver
// @Router /api/v1/change-requests/{id}/approvers/{approverId}/promote [post]
func (h *Handler) promote(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	watcherID, ok := parseUUIDParam(w, r, "approverId")
	if !ok {
		return
	}
	ap, err := h.svc.PromoteWatcherToApprover(r.Context(), crID, watcherID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, ap)
}

// demote godoc
// @Summary Demote an approver to watcher
// @Success 201 {object} Watcher
// @Router /api/v1/change-requests/{id}/approvers/{approverId}/demote [post]
func (h *Handler) demote(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	approverID, ok := parseUUIDParam(w, r, "approverId")
	if !ok {
		return
	}
	wt, err := h.svc.DemoteApproverToWatcher(r.Context(), crID, approverID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, wt)
}

// approverCandidates godoc
// @Summary List users eligible as approvers (not already participating)
// @Success 200 {array} map[string]any
// @Router /api/v1/change-requests/{id}/approver-candidates [get]
func (h *Handler) approverCandidates(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ApproverCandidates(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []map[string]any{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// requireActor extracts the authenticated user, writing a 401 when absent.
func requireActor(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "not authenticated"))
		return uuid.Nil, false
	}
	return id, true
}

func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	return parseUUIDParam(w, r, "id")
}

func parseUUIDParam(w http.ResponseWriter, r *http.Request, key string) (uuid.UUID, bool) {
	raw := chi.URLParam(r, key)
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid "+key+" (must be a UUID)"))
		return uuid.Nil, false
	}
	return id, true
}
