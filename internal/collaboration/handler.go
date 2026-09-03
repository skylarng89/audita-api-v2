package collaboration

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/audita/audita-api/internal/platform/storage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// MaxUploadBytes caps attachment uploads at the handler layer; the config
// value is injected at construction.
const MaxUploadBytes int64 = 25 << 20

// Handler exposes collaboration REST endpoints under the CR resource.
type Handler struct {
	svc    *Service
	store  storage.Store
}

// NewHandler builds a collaboration Handler.
func NewHandler(svc *Service, store storage.Store) *Handler {
	return &Handler{svc: svc, store: store}
}

// Routes registers comment/attachment/activity endpoints under an
// authenticated router. Intended to be mounted at /change-requests/{id}.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/comments", h.listComments)
	r.Post("/comments", h.addComment)
	r.Get("/attachments", h.listAttachments)
	r.Post("/attachments", h.addAttachment)
	r.Get("/attachments/{attachmentId}/download", h.download)
	r.Get("/activity", h.listActivity)
}

// addComment godoc
// @Summary Add a comment (sanitized; @mentions trigger watcher enrollment)
// @Accept json
// @Success 201 {object} Comment
// @Router /api/v1/change-requests/{id}/comments [post]
func (h *Handler) addComment(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	c, err := h.svc.AddComment(r.Context(), actorID, crID, req.Body)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, c)
}

func (h *Handler) listComments(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListComments(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*Comment{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// addAttachment godoc
// @Summary Upload an attachment (multipart/form-data, field "file")
// @Success 201 {object} Attachment
// @Router /api/v1/change-requests/{id}/attachments [post]
func (h *Handler) addAttachment(w http.ResponseWriter, r *http.Request) {
	actorID, ok := requireActor(w, r)
	if !ok {
		return
	}
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(MaxUploadBytes); err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid multipart form (or file too large)"))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "missing 'file' field"))
		return
	}
	defer func() { _ = file.Close() }()

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	key := storage.RandomKey(header.Filename)
	if err := h.store.Put(r.Context(), key, MaxUploadBytes, mimeType, file); err != nil {
		if errors.Is(err, storage.ErrTooLarge) {
			httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "attachment exceeds the size limit"))
			return
		}
		httpx.RespondProblem(w, r, err)
		return
	}
	a, err := h.svc.AddAttachment(r.Context(), actorID, crID, header.Filename, mimeType, header.Size, key)
	if err != nil {
		_ = h.store.Delete(r.Context(), key) // best-effort cleanup
		httpx.RespondProblem(w, r, err)
		return
	}
	httpx.RespondJSON(w, http.StatusCreated, a)
}

// download godoc
// @Summary Download an attachment
// @Router /api/v1/change-requests/{id}/attachments/{attachmentId}/download [get]
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	attachmentID, ok := parseUUIDParam(w, r, "attachmentId")
	if !ok {
		return
	}
	atts, err := h.svc.ListAttachments(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	var target *Attachment
	for _, a := range atts {
		if a.ID == attachmentID {
			target = a
			break
		}
	}
	if target == nil {
		httpx.RespondProblem(w, r, ErrNotFound)
		return
	}
	rc, err := h.store.Get(r.Context(), target.StorageKey)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", target.MimeType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+target.FileName+`"`)
	w.Header().Set("Content-Length", itoa(target.SizeBytes))
	_, _ = io.Copy(w, rc)
}

func (h *Handler) listAttachments(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListAttachments(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*Attachment{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

func (h *Handler) listActivity(w http.ResponseWriter, r *http.Request) {
	crID, ok := parseID(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListActivity(r.Context(), crID)
	if err != nil {
		httpx.RespondProblem(w, r, err)
		return
	}
	if list == nil {
		list = []*ActivityEntry{}
	}
	httpx.RespondJSON(w, http.StatusOK, list)
}

// requireActor mirrors changerequest's guard.
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

// itoa avoids strconv import noise in a single-use spot.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
