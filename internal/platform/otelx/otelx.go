// Package otelx bootstraps OpenTelemetry tracing (blueprint §4.2, Task 6.3).
package otelx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerProvider is the subset the app needs for shutdown + tracer access.
type TracerProvider struct {
	inner *sdktrace.TracerProvider
}

// Setup initialises the global tracer provider with an OTLP HTTP exporter
// when OTEL_EXPORTER_OTLP_ENDPOINT is configured; otherwise it returns a
// no-op provider so tracing is cheap and disabled by default.
func Setup(ctx context.Context, serviceName, endpoint string, samplingProbability float64, logger *slog.Logger) (*TracerProvider, error) {
	// Always install the W3C propagator so trace context crosses HTTP and
	// NATS message headers consistently.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if strings.TrimSpace(endpoint) == "" {
		logger.Info("otel: no OTEL_EXPORTER_OTLP_ENDPOINT configured; tracing disabled")
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return &TracerProvider{}, nil
	}

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")),
	}
	if strings.HasPrefix(endpoint, "http://") {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otelx: exporter: %w", err)
	}
	if samplingProbability <= 0 || samplingProbability > 1 {
		samplingProbability = 0.1
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("otelx: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(samplingProbability))),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(tp)
	hostname, _ := os.Hostname()
	logger.Info("otel: tracing enabled",
		slog.String("endpoint", endpoint), slog.String("host", hostname))
	return &TracerProvider{inner: tp}, nil
}

// Shutdown flushes pending spans; call on graceful shutdown.
func (t *TracerProvider) Shutdown(ctx context.Context) error {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Shutdown(ctx)
}
