package collaboration

import (
	"context"

	"github.com/google/uuid"
)

// TenantFromContext retrieves the tenant UUID set by the tenancy middleware.
var tenantLookup = func(ctx context.Context) (uuid.UUID, bool) {
	return uuid.Nil, false
}

// TenantFromContext is the module-level accessor.
func TenantFromContext(ctx context.Context) (uuid.UUID, bool) {
	return tenantLookup(ctx)
}

// SetTenantLookup wires the extractor; called once from cmd/api.
func SetTenantLookup(fn func(ctx context.Context) (uuid.UUID, bool)) {
	tenantLookup = fn
}
