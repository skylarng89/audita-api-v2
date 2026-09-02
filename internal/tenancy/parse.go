package tenancy

import (
	"net/http"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
)

// parseUUID extracts and parses a chi URL parameter into a UUID, writing a
// 400 Problem on failure. Returns (uuid, ok).
func parseUUID(w http.ResponseWriter, r *http.Request, key string) (uuid.UUID, bool) {
	raw := RouteCtx(r, key)
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(
			http.StatusBadRequest, httpx.CodeValidation, "invalid "+key+" (must be a UUID)"))
		return uuid.Nil, false
	}
	return id, true
}
