package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	events "github.com/discordia-grupo01/events-contract/billing"
	"github.com/discordia-grupo01/events-contract/boosts"
	"github.com/discordia-grupo01/events-contract/envelope"

	"github.com/discordia-grupo01/billing/internal/model"
)

const secret = "test-secret"

var errBoom = errors.New("boom")

// fakeStore hands out its events in order, like PublishNext: an event is
// removed only if publish succeeds.
type fakeStore struct {
	pending []model.OutboxEvent
	err     error
}

func (f *fakeStore) PublishNext(ctx context.Context, publish func(context.Context, model.OutboxEvent) error) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if len(f.pending) == 0 {
		return false, nil
	}
	if err := publish(ctx, f.pending[0]); err != nil {
		return true, err
	}
	f.pending = f.pending[1:]
	return true, nil
}

type sent struct {
	key  string
	body []byte
}

type fakePublisher struct {
	sent []sent
	err  error
}

func (f *fakePublisher) Publish(_ context.Context, key string, body []byte) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sent{key, body})
	return nil
}

func premiumEvent(t *testing.T, version int64) model.OutboxEvent {
	t.Helper()
	until := time.Date(2026, 12, 3, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(events.PremiumUpdated{
		Meta:   envelope.Meta{EventID: uuid.NewString(), OccurredAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)},
		UserID: uuid.NewString(), Active: true, PremiumUntil: &until, Renews: true, Version: version,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return model.OutboxEvent{ID: uuid.New(), RoutingKey: events.RoutingKeyPremiumUpdated, Payload: payload}
}

func TestDrainSignsAndPublishes(t *testing.T) {
	store := &fakeStore{pending: []model.OutboxEvent{premiumEvent(t, 1), premiumEvent(t, 2)}}
	pub := &fakePublisher{}

	n, err := NewRelay(store, pub, secret).Drain(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("Drain = %d, %v; want 2, nil", n, err)
	}
	for i, s := range pub.sent {
		if s.key != events.RoutingKeyPremiumUpdated {
			t.Errorf("event %d: routing key %q", i, s.key)
		}
		var got events.PremiumUpdated
		if err := json.Unmarshal(s.body, &got); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if got.Version != int64(i+1) || !got.Verify(secret) {
			t.Errorf("event %d should be published signed: %s", i, s.body)
		}
	}
}

func TestDrainStopsAtTheFirstFailure(t *testing.T) {
	store := &fakeStore{pending: []model.OutboxEvent{premiumEvent(t, 1), premiumEvent(t, 2)}}
	pub := &fakePublisher{err: errBoom}

	n, err := NewRelay(store, pub, secret).Drain(context.Background())
	if !errors.Is(err, errBoom) || n != 0 {
		t.Fatalf("Drain = %d, %v; want 0, boom", n, err)
	}
	if len(store.pending) != 2 {
		t.Error("nothing should be marked published")
	}
}

func TestDrainBoundsOneRun(t *testing.T) {
	store := &fakeStore{}
	for i := range batchSize + 5 {
		store.pending = append(store.pending, premiumEvent(t, int64(i+1)))
	}
	n, err := NewRelay(store, &fakePublisher{}, secret).Drain(context.Background())
	if err != nil || n != batchSize || len(store.pending) != 5 {
		t.Errorf("Drain = %d, %v with %d left; want %d and 5 left", n, err, len(store.pending), batchSize)
	}
}

func TestDrainStoreError(t *testing.T) {
	_, err := NewRelay(&fakeStore{err: errBoom}, &fakePublisher{}, secret).Drain(context.Background())
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestDrainNeverPublishesUnsigned(t *testing.T) {
	tests := map[string]model.OutboxEvent{
		"unknown routing key": {ID: uuid.New(), RoutingKey: "billing.something_new", Payload: []byte(`{}`)},
		"invalid payload":     {ID: uuid.New(), RoutingKey: events.RoutingKeyPremiumUpdated, Payload: []byte(`{`)},
	}
	for name, e := range tests {
		t.Run(name, func(t *testing.T) {
			pub := &fakePublisher{}
			_, err := NewRelay(&fakeStore{pending: []model.OutboxEvent{e}}, pub, secret).Drain(context.Background())
			if err == nil || len(pub.sent) != 0 {
				t.Errorf("err = %v, sent %d; want an error and nothing sent", err, len(pub.sent))
			}
		})
	}
}

func TestRelaySignsEveryBoostEventWithItsRoutingKey(t *testing.T) {
	for _, key := range []string{boosts.RoutingKeyRequested, boosts.RoutingKeyCancelled, boosts.RoutingKeyRemoved} {
		t.Run(key, func(t *testing.T) {
			payload, err := json.Marshal(boosts.SagaEvent{
				Meta:     envelope.Meta{EventID: "evt-1", OccurredAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)},
				BoostID:  uuid.NewString(),
				ServerID: uuid.NewString(),
				UserID:   uuid.NewString(),
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := NewRelay(nil, nil, secret).sign(model.OutboxEvent{ID: uuid.New(), RoutingKey: key, Payload: payload})
			if err != nil {
				t.Fatalf("sign() error = %v", err)
			}
			var evt boosts.SagaEvent
			if err := json.Unmarshal(got, &evt); err != nil {
				t.Fatal(err)
			}
			if !evt.Verify(key, secret) {
				t.Error("the published event doesn't verify")
			}
			if evt.Verify(boosts.RoutingKeyApplied, secret) {
				t.Error("it verified under another routing key")
			}
		})
	}
}

func TestRelayRejectsABadBoostPayload(t *testing.T) {
	_, err := NewRelay(nil, nil, secret).sign(model.OutboxEvent{ID: uuid.New(), RoutingKey: boosts.RoutingKeyRequested, Payload: []byte(`nope`)})
	if err == nil {
		t.Error("expected an error")
	}
}
