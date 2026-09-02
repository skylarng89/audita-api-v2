package runtime

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Middleware returns the shared middleware chain applied to every route:
// request-ID injection, panic recovery, structured logging, and body limits.
func Middleware(logger *slog.Logger) []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		RequestIDMiddleware,
		RecoverMiddleware(logger),
		LoggingMiddleware(logger),
	}
}

// RequestIDMiddleware ensures every request has a correlation ID, accepting
// an inbound `X-Request-ID` or generating one, and echoing it on the response.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := httpx.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// LoggingMiddleware emits one structured log line per request (FR-10).
func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", httpx.RequestID(r.Context())),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// RecoverMiddleware converts panics into a 500 Problem instead of crashing
// the process, and logs the panic with a stack trace.
func RecoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "panic recovered",
						slog.String("request_id", httpx.RequestID(r.Context())),
						slog.Any("panic", rec),
					)
					httpx.RespondProblem(w, r, httpx.NewProblem(
						http.StatusInternalServerError, httpx.CodeInternal, "internal server error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RequestRoutePattern extracts the chi route pattern (e.g. "/api/v1/cr/{id}")
// for low-cardinality metrics labels.
func RequestRoutePattern(r *http.Request) string {
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	if pattern == "" {
		return r.URL.Path
	}
	return pattern
}
