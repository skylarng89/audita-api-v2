// Package httpx provides shared HTTP request/response helpers for chi-based
// handlers across all modules (ADR-2: stdlib-first with shared helpers to
// reduce boilerplate).
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxBodyBytes is the default request-body cap for JSON endpoints.
const MaxBodyBytes int64 = 2 << 20 // 2 MiB

// Problem is an RFC 7807-style structured error payload returned by all API
// endpoints so the frontend can render consistent messages (e.g. the v1
// DUPLICATE_PARTICIPANT contract).
type Problem struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
	RequestID  string         `json:"requestId,omitempty"`
	HTTPStatus int            `json:"-"`
}

// Error implements the error interface so a Problem can be returned from
// service methods and rendered uniformly by handlers.
func (p *Problem) Error() string {
	return fmt.Sprintf("[%s] %s", p.Code, p.Message)
}

// NewProblem builds a Problem with the given status/code/message.
func NewProblem(status int, code, message string) *Problem {
	return &Problem{Code: code, Message: message, Timestamp: time.Now().UTC(), HTTPStatus: status}
}

// Common problem codes shared across modules.
const (
	CodeBadRequest            = "BAD_REQUEST"
	CodeValidation            = "VALIDATION_ERROR"
	CodeUnauthorized          = "UNAUTHORIZED"
	CodeForbidden             = "FORBIDDEN"
	CodeNotFound              = "NOT_FOUND"
	CodeConflict              = "CONFLICT"
	CodeDuplicateParticipant  = "DUPLICATE_PARTICIPANT"
	CodeInvalidTransition     = "INVALID_STATE_TRANSITION"
	CodeNotPermitted          = "DOMAIN_NOT_PERMITTED"
	CodeRateLimited           = "RATE_LIMITED"
	CodeIdempotencyConflict   = "IDEMPOTENCY_KEY_CONFLICT"
	CodeInternal              = "INTERNAL_ERROR"
	CodeServiceUnavailable    = "SERVICE_UNAVAILABLE"
)

// DecodeJSON decodes a JSON request body into dst, enforcing a size cap and
// rejecting unknown fields to catch client contract drift early.
func DecodeJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return NewProblem(http.StatusBadRequest, CodeBadRequest, "invalid JSON body: "+err.Error())
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return NewProblem(http.StatusBadRequest, CodeBadRequest, "body must contain a single JSON object")
	}
	return nil
}

// RespondJSON writes dst as JSON with the given status code.
func RespondJSON(w http.ResponseWriter, status int, dst any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if dst == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(dst)
}

// RespondProblem writes a Problem as an RFC7807-style error response.
// It accepts any error; *Problem is rendered directly, other errors become a
// 500 with a generic message (never leaking internals to the client).
func RespondProblem(w http.ResponseWriter, r *http.Request, err error) {
	var prob *Problem
	if !errors.As(err, &prob) {
		prob = NewProblem(http.StatusInternalServerError, CodeInternal, "internal server error")
	}
	if prob.RequestID == "" {
		prob.RequestID = RequestID(r.Context())
	}
	if prob.HTTPStatus == 0 {
		prob.HTTPStatus = http.StatusInternalServerError
	}
	RespondJSON(w, prob.HTTPStatus, prob)
}

// contextKey is an unexported type for context keys in this package.
type contextKey int

const (
	ctxKeyRequestID contextKey = iota
	ctxKeyTenantID
	ctxKeyUserID
	ctxKeyRole
)

// WithRequestID stores a request correlation ID in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID retrieves the request correlation ID from ctx ("" if unset).
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithTenantID stores the resolved tenant UUID in ctx (set by the tenancy
// middleware after X-Tenant-Slug resolution).
func WithTenantID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyTenantID, id)
}

// TenantID retrieves the tenant UUID from ctx.
func TenantID(ctx context.Context) (uuid.UUID, bool) {
	if v, ok := ctx.Value(ctxKeyTenantID).(uuid.UUID); ok {
		return v, true
	}
	return uuid.Nil, false
}

// WithUserID stores the authenticated user's UUID in ctx (set by the identity
// middleware after JWT verification).
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}

// UserID retrieves the authenticated user's UUID from ctx.
func UserID(ctx context.Context) (uuid.UUID, bool) {
	if v, ok := ctx.Value(ctxKeyUserID).(uuid.UUID); ok {
		return v, true
	}
	return uuid.Nil, false
}

// WithRole stores the authenticated user's primary role in ctx.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, ctxKeyRole, role)
}

// Role retrieves the authenticated user's primary role from ctx.
func Role(ctx context.Context) (string, bool) {
	if v, ok := ctx.Value(ctxKeyRole).(string); ok && v != "" {
		return v, true
	}
	return "", false
}

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}
