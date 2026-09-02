package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// NewLogger builds the process-wide structured logger (FR-10).
// Level is controlled by LOG_LEVEL_APP (debug|info|warn|error).
func NewLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(handler).With(slog.String("service", "audita-api"))
}

// NewNopLogger returns a logger that discards output (used in tests).
func NewNopLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Fatalf logs a fatal error and exits — used at startup for unrecoverable
// configuration/infra failures only.
func Fatalf(logger *slog.Logger, format string, args ...any) {
	logger.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// BackgroundJob runs fn on a ticker until ctx is cancelled, recovering from
// panics so a single bad iteration cannot kill the scheduler goroutine
// (blueprint §4.3 DB resilience).
func BackgroundJob(ctx context.Context, logger *slog.Logger, name string, interval time.Duration, fn func(ctx context.Context) error) context.CancelFunc {
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							logger.Error("background job panic", slog.String("job", name), slog.Any("panic", rec))
						}
					}()
					if err := fn(cctx); err != nil {
						logger.Error("background job failed", slog.String("job", name), slog.String("error", err.Error()))
					}
				}()
			}
		}
	}()
	return cancel
}
