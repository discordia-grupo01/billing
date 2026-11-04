// Package outbox publishes the events billing wrote to outbox_events.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	events "github.com/discordia-grupo01/events-contract/billing"

	"github.com/discordia-grupo01/billing/internal/model"
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
	return r.publisher.Publish(ctx, e.RoutingKey, body)
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
	default:
		return nil, fmt.Errorf("outbox: no signer for routing key %q", e.RoutingKey)
	}
}
