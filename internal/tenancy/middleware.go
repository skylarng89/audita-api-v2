package tenancy

import (
	"net/http"
	"strings"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Middleware injects tenant resolution into every tenant-scoped request:
// reads `X-Tenant-Slug` (falling back to subdomain), resolves it to a
// Tenant, verifies it is active, and stores the tenant UUID in ctx
// (FR-3 / ADR-4). It runs BEFORE identity middleware so JWT validation can
// check tenant claims against it.
func Middleware(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug := resolveSlug(r)
			if slug == "" {
				httpx.RespondProblem(w, r, httpx.NewProblem(
					http.StatusBadRequest, "TENANT_REQUIRED", "missing X-Tenant-Slug header"))
				return
			}
			t, err := svc.GetBySlug(r.Context(), slug)
			if err != nil {
				httpx.RespondProblem(w, r, err)
				return
			}
			if t.Status != StatusActive {
				httpx.RespondProblem(w, r, httpx.NewProblem(
					http.StatusForbidden, "TENANT_INACTIVE", "tenant is not active"))
				return
			}
			ctx := httpx.WithTenantID(r.Context(), t.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveSlug extracts the tenant slug from the X-Tenant-Slug header, falling
// back to the first DNS label of the Host header (wildcard-host deployments).
func resolveSlug(r *http.Request) string {
	if slug := strings.TrimSpace(r.Header.Get("X-Tenant-Slug")); slug != "" {
		return slug
	}
	host := strings.Split(r.Host, ":")[0]
	parts := strings.SplitN(host, ".", 2)
	if len(parts) == 2 && parts[0] != "www" && parts[0] != "api" {
		return parts[0]
	}
	return ""
}

// RouteCtx extracts the tenant path/query value set by chi route params,
// used by platform routes like /platform/tenants/{id}.
func RouteCtx(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}
