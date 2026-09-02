// Package health provides liveness/readiness endpoints shared by all
// modules (/healthz, /readyz) and the Prometheus /metrics scrape target.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
)

// Checker reports component health for readiness.
type Checker func(ctx context.Context) error

// Handler serves /healthz (liveness) and /readyz (readiness with checks).
type Handler struct {
	checks map[string]Checker
}

// NewHandler builds a Handler with named readiness checks.
func NewHandler(checks map[string]Checker) *Handler {
	return &Handler{checks: checks}
}

// Register mounts health endpoints on the given mux/subrouter.
func (h *Handler) Register(r interface {
	Get(pattern string, handlerFn http.HandlerFunc)
}) {
	r.Get("/healthz", h.liveness)
	r.Get("/readyz", h.readiness)
}

func (h *Handler) liveness(w http.ResponseWriter, _ *http.Request) {
	httpx.RespondJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (h *Handler) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	failed := make(map[string]string, len(h.checks))
	for name, check := range h.checks {
		if err := check(ctx); err != nil {
			failed[name] = err.Error()
		}
	}
	status := http.StatusOK
	state := "ready"
	if len(failed) > 0 {
		status = http.StatusServiceUnavailable
		state = "degraded"
	}
	httpx.RespondJSON(w, status, map[string]any{
		"status":  state,
		"checks":  failed,
		"time":    time.Now().UTC(),
	})
}
