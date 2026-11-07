package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type scrubbingExporter struct{ sdktrace.SpanExporter }

func (e scrubbingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	scrubbed := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		scrubbed[i] = scrubbedSpan{span}
	}
	return e.SpanExporter.ExportSpans(ctx, scrubbed)
}

type scrubbedSpan struct{ sdktrace.ReadOnlySpan }

func (s scrubbedSpan) Events() []sdktrace.Event {
	events := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(events))
	for i, event := range events {
		kept := make([]attribute.KeyValue, 0, len(event.Attributes))
		for _, attr := range event.Attributes {
			if attr.Key == "exception.message" || attr.Key == "exception.stacktrace" {
				continue
			}
			kept = append(kept, attr)
		}
		event.Attributes = kept
		out[i] = event
	}
	return out
}
