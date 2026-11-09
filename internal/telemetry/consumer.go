package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	amqp "github.com/rabbitmq/amqp091-go"
)

func StartConsumer(ctx context.Context, queue string, delivery amqp.Delivery) (context.Context, func(failed bool)) {
	ctx = ExtractAMQP(ctx, delivery.Headers)
	ctx, span := otel.Tracer(tracerName).Start(ctx, delivery.RoutingKey+" process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", queue),
			attribute.String("messaging.rabbitmq.destination.routing_key", delivery.RoutingKey),
		),
	)
	return ctx, func(failed bool) {
		if failed {
			span.SetStatus(codes.Error, "processing failed")
		}
		span.End()
	}
}
