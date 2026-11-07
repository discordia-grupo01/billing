package telemetry

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	tracerName  = "github.com/discordia-grupo01/billing"
)

func Enabled() bool {
	return os.Getenv(EnvEndpoint) != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

func Setup(ctx context.Context, service string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if !Enabled() {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: cannot create the OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", service)),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: cannot build the resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(scrubbingExporter{exporter}),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)

	return provider.Shutdown, nil
}
