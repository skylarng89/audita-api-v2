package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"
	"github.com/nats-io/nats.go"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/audita/audita-api/internal/identity"
	"github.com/audita/audita-api/internal/notification"
	"github.com/audita/audita-api/internal/platform/runtime"
)

// ── HTML sanitization (collaboration module dependency) ─────────────────

// bluemondayPolicy allows the rich-text subset the Tiptap editor produces
// (paragraphs, headings, links with safe schemes, lists, emphasis, code,
// task lists) and strips everything else — the Go equivalent of v1's OWASP
// sanitizer (blueprint §4.4).
var bluemondayPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("h1", "h2", "h3", "hr", "br")
	p.AllowAttrs("data-type", "data-id").OnElements("span")
	p.AllowElements("pre", "code")
	return p
}()

// sanitizeHTML strips disallowed markup from user-supplied rich text.
func sanitizeHTML(raw string) string {
	return bluemondayPolicy.Sanitize(strings.TrimSpace(raw))
}

// ── Cross-module adapters (composition-root glue) ────────────────────────

// wsTokenAdapter adapts identity.TokenService to the notification module's
// narrow TokenVerifier port (ADR-1 boundary discipline).
type wsTokenAdapter struct{ inner *identity.TokenService }

func (a wsTokenAdapter) Verify(token string) (notification.TokenClaims, error) {
	claims, err := a.inner.Verify(token)
	if err != nil {
		return notification.TokenClaims{}, err
	}
	return notification.TokenClaims{UserID: claims.UserID}, nil
}

// wsMetricsAdapter adapts runtime.Metrics to the gateway's recorder port.
type wsMetricsAdapter struct{ inner *runtime.Metrics }

func (a wsMetricsAdapter) SetWSConnections(n float64) {
	a.inner.WebSocketActiveConnections.Set(n)
}

// natsHeaderCarrier is retained for future direct (non-outbox) publishes
// that need trace-context propagation through NATS headers.
type natsHeaderCarrier struct{ hdr nats.Header }

func (c natsHeaderCarrier) Get(key string) string { return c.hdr.Get(key) }
func (c natsHeaderCarrier) Set(key, value string) { c.hdr.Set(key, value) }
func (c natsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c.hdr))
	for k := range c.hdr {
		keys = append(keys, k)
	}
	return keys
}

// uuidPattern documents the @mention token format shared with the frontend.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ctxGetter is a named type making the tenancy bridge explicit.
type ctxGetter = func(ctx context.Context) (uuid.UUID, bool)

var _ ctxGetter = httpxTenantID

func httpxTenantID(ctx context.Context) (uuid.UUID, bool) {
	return httpx.TenantID(ctx)
}
