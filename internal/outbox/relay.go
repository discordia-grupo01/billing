// Package outbox publishes the events billing wrote to outbox_events.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	events "github.com/discordia-grupo01/events-contract/billing"
	"github.com/discordia-grupo01/events-contract/boosts"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/discordia-grupo01/billing/internal/logging"
	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/telemetry"
	"github.com/discordia-grupo01/events-contract/validate"
)

// batchSize bounds one run, so a big backlog doesn't hold a run forever.
const batchSize = 100

type Store interface {
	PublishNext(ctx context.Context, publish func(context.Context, model.OutboxEvent) error) (bool, error)
}

type Publisher interface {
	Publish(ctx context.Context, routingKey string, body []byte) error
}

// Relay signs and publishes outbox events. Every billing event is signed:
// one without a signer is never published, so a new event type can't leak
// out unsigned by mistake.
type Relay struct {
	store     Store
	publisher Publisher
	secret    string
}

func NewRelay(store Store, publisher Publisher, secret string) *Relay {
	return &Relay{store: store, publisher: publisher, secret: secret}
}

// Drain publishes up to batchSize events and returns how many it published.
// It stops at the first failure: the broker is most likely down, and the
// failed event is retried on the next run.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	published := 0
	for range batchSize {
		found, err := r.store.PublishNext(ctx, r.publish)
		if err != nil {
			return published, err
		}
		if !found {
			break
		}
		published++
	}
	return published, nil
}

func (r *Relay) publish(ctx context.Context, e model.OutboxEvent) error {
	body, err := r.sign(e)
	if err != nil {
		return err
	}
	if verr := validate.Validate(e.RoutingKey, body); verr != nil && !errors.Is(verr, validate.ErrUnknownEvent) {
		logging.Warnf("outbox: %q (id=%s) does not meet the event contract: %v", e.RoutingKey, e.ID, verr)
	}

	publishCtx := telemetry.ContextWithTraceparent(ctx, e.Traceparent)
	publishCtx, span := otel.Tracer("github.com/discordia-grupo01/billing").Start(publishCtx, "outbox publish "+e.RoutingKey,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", e.RoutingKey),
		),
	)
	defer span.End()
	if err := r.publisher.Publish(publishCtx, e.RoutingKey, body); err != nil {
		span.SetStatus(codes.Error, "publish failed")
		return err
	}
	return nil
}

func (r *Relay) sign(e model.OutboxEvent) ([]byte, error) {
	switch e.RoutingKey {
	case events.RoutingKeyPremiumUpdated:
		var evt events.PremiumUpdated
		if err := json.Unmarshal(e.Payload, &evt); err != nil {
			return nil, fmt.Errorf("outbox: decode %s %s: %w", e.RoutingKey, e.ID, err)
		}
		evt.Sign(r.secret)
		return json.Marshal(evt)
	case boosts.RoutingKeyRequested, boosts.RoutingKeyCancelled, boosts.RoutingKeyRemoved:
		var evt boosts.SagaEvent
		if err := json.Unmarshal(e.Payload, &evt); err != nil {
			return nil, fmt.Errorf("outbox: decode %s %s: %w", e.RoutingKey, e.ID, err)
		}
		evt.Sign(e.RoutingKey, r.secret)
		return json.Marshal(evt)
	default:
		return nil, fmt.Errorf("outbox: no signer for routing key %q", e.RoutingKey)
	}
}
