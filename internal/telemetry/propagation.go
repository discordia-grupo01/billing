package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	amqp "github.com/rabbitmq/amqp091-go"
)

const traceparentKey = "traceparent"

func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get(traceparentKey)
}

func ContextWithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{traceparentKey: traceparent})
}

func InjectAMQP(ctx context.Context, headers amqp.Table) amqp.Table {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return headers
	}
	if headers == nil {
		headers = amqp.Table{}
	}
	otel.GetTextMapPropagator().Inject(ctx, amqpCarrier(headers))
	return headers
}

func ExtractAMQP(ctx context.Context, headers amqp.Table) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, amqpCarrier(headers))
}

type amqpCarrier amqp.Table

func (c amqpCarrier) Get(key string) string {
	if value, ok := c[key].(string); ok {
		return value
	}
	return ""
}

func (c amqpCarrier) Set(key, value string) { c[key] = value }

func (c amqpCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}
