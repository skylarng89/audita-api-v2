package changerequest

import (
	"context"

	"github.com/google/uuid"
)

// TenantFromContext retrieves the tenant UUID set by the tenancy middleware.
// Declared here (rather than imported from httpx) so this module's context
// helpers stay self-contained; cmd/api bridges the two.
func TenantFromContext(ctx context.Context) (uuid.UUID, bool) {
	return tenantLookup(ctx)
}

// tenantLookup is injected by the composition root to avoid a direct
// dependency on internal/httpx from this module's public surface.
var tenantLookup = func(ctx context.Context) (uuid.UUID, bool) {
	return uuid.Nil, false
}

// SetTenantLookup wires the tenant extractor; called once from cmd/api.
func SetTenantLookup(fn func(ctx context.Context) (uuid.UUID, bool)) {
	tenantLookup = fn
}
