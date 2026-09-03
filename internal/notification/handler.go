package notification

import (
	"net/http"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes notification REST endpoints + the WS upgrade route.
type Handler struct {
	svc       *Service
	gateway   *Gateway
	wsHandler http.HandlerFunc
}

// NewHandler builds a notification Handler.
func NewHandler(svc *Service, gateway *Gateway) *Handler {
	return &Handler{svc: svc, gateway: gateway}
}

// BindWSHandler wires the gateway upgrade handler (bound in cmd/api with a
// TokenVerifier). Must be called before Routes.
func (h *Handler) BindWSHandler(fn http.HandlerFunc) {
	h.wsHandler = fn
}

// Routes registers notification endpoints (auth required).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/notifications", h.list)
	r.Get("/notifications/unread-count", h.unreadCount)
	r.Post("/notifications/{id}/read", h.markRead)
	r.Post("/notifications/read-all", h.markAllRead)
	// WS upgrade; token query param authenticates the handshake (ADR-6).
	r.Get("/notifications/stream", h.stream)
}

// list godoc
// @Summary List my notifications
// @Produce json
// @Success 200 {array} Notification
// @Router /api/v1/notifications [get]
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireActor(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), userID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*Notification{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// unreadCount godoc
// @Summary Count my unread notifications
// @Success 200 {object} map[string]int
// @Router /api/v1/notifications/unread-count [get]
func (h *Handler) unreadCount(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireActor(w, r)
	if !ok {
		return
	}
	n, err := h.svc.UnreadCount(r.Context(), userID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusOK, map[string]int{"count": n})
}

// markRead godoc
// @Summary Mark one notification read
// @Success 204
// @Router /api/v1/notifications/{id}/read [post]
func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireActor(w, r)
	if !ok {
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid id"))
		return
	}
	if err := h.svc.MarkRead(r.Context(), userID, id); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// markAllRead godoc
// @Summary Mark all my notifications read
// @Success 204
// @Router /api/v1/notifications/read-all [post]
func (h *Handler) markAllRead(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireActor(w, r)
	if !ok {
		return
	}
	if err := h.svc.MarkAllRead(r.Context(), userID); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stream godoc — WebSocket upgrade (not an SSE stream; ADR-6 upgrade).
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	if h.wsHandler != nil {
		h.wsHandler(w, r)
		return
	}
	httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, httpx.CodeServiceUnavailable, "websocket gateway unavailable"))
}
