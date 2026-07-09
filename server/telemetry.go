// telemetry.go is the module SDK's OpenTelemetry OTLP trace wiring. It is entirely
// gated on OTEL_EXPORTER_OTLP_ENDPOINT: empty ⇒ initTelemetry installs nothing and
// returns a no-op shutdown, so a module that isn't asked to export traces behaves
// exactly as before. When set, it builds an OTLP/gRPC exporter, a resource with
// service.name, a batching TracerProvider, and the W3C TraceContext + Baggage
// propagator the otelgrpc stats handler on this server needs to actually propagate.
//
// This file is COPIED verbatim into each module alongside server.go, mirroring how the
// rest of the SDK is vendored per module — each foundations component is its own Go
// module, so the telemetry init is duplicated, not shared.
package server

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// telemetryShutdown flushes and stops the TracerProvider; deferred in Serve so batched
// spans are exported at process exit.
type telemetryShutdown func(context.Context) error

func noopTelemetryShutdown(context.Context) error { return nil }

// initTelemetry wires OTLP trace export when OTEL_EXPORTER_OTLP_ENDPOINT is set, else
// installs nothing. serviceName defaults to the module token (OTEL_SERVICE_NAME, via the
// resource's WithFromEnv, still wins). Returns a shutdown func to defer.
func initTelemetry(ctx context.Context, serviceName string) (telemetryShutdown, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return noopTelemetryShutdown, nil
	}
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return noopTelemetryShutdown, err
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		res = resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(serviceName))
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}
