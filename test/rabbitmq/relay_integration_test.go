package rabbitmq_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	events "github.com/discordia-grupo01/events-contract/billing"
	"github.com/discordia-grupo01/events-contract/envelope"

	"github.com/discordia-grupo01/billing/internal/outbox"
	"github.com/discordia-grupo01/billing/internal/rabbitmq"
	"github.com/discordia-grupo01/billing/internal/repository"
)

// An event written to the outbox reaches RabbitMQ signed, and a consumer
// with the shared secret can verify it.
func TestRelayPublishesSignedEventsEndToEnd(t *testing.T) {
	url := rabbitURL(t)
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set: skipping integration test")
	}
	ctx := context.Background()
	pool, err := repository.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	if strings.Contains(dsn, "supabase") {
		t.Fatal("TEST_DATABASE_URL points to Supabase; use a disposable local Postgres")
	}
	for _, file := range []string{"0001_init.down.sql", "0001_init.up.sql", "0006_outbox_traceparent.up.sql"} {
		sql, err := os.ReadFile("../../migrations/" + file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}

	until := time.Now().Add(30 * 24 * time.Hour).UTC()
	payload, _ := json.Marshal(events.PremiumUpdated{
		Meta:   envelope.Meta{EventID: uuid.NewString(), OccurredAt: time.Now()},
		UserID: uuid.NewString(), Active: true, PremiumUntil: &until, Renews: true, Version: 1,
	})
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id, routing_key, payload) VALUES ($1, $2, $3)`,
		uuid.New(), events.RoutingKeyPremiumUpdated, payload); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	deliveries := listen(t, url, events.RoutingKeyPremiumUpdated)
	publisher := rabbitmq.NewPublisher(url)
	t.Cleanup(publisher.Close)
	relay := outbox.NewRelay(repository.NewOutboxRepository(pool), publisher, "shared-secret")

	if n, err := relay.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("Drain = %d, %v; want 1, nil", n, err)
	}
	var got events.PremiumUpdated
	if err := json.Unmarshal(receive(t, deliveries).Body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Verify("shared-secret") || !got.Active || got.Version != 1 {
		t.Errorf("event should arrive signed and intact: %+v", got)
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Errorf("pending events = %d, %v; want 0", pending, err)
	}
}

func TestRelayPublishesInsideTheTraceStoredWithTheEvent(t *testing.T) {
	url := rabbitURL(t)
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set: skipping integration test")
	}
	ctx := context.Background()
	pool, err := repository.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	if strings.Contains(dsn, "supabase") {
		t.Fatal("TEST_DATABASE_URL points to Supabase; use a disposable local Postgres")
	}
	for _, file := range []string{"0001_init.down.sql", "0001_init.up.sql", "0006_outbox_traceparent.up.sql"} {
		sql, err := os.ReadFile("../../migrations/" + file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}

	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	until := time.Now().Add(30 * 24 * time.Hour).UTC()
	payload, _ := json.Marshal(events.PremiumUpdated{
		Meta:   envelope.Meta{EventID: uuid.NewString(), OccurredAt: time.Now()},
		UserID: uuid.NewString(), Active: true, PremiumUntil: &until, Renews: true, Version: 1,
	})
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id, routing_key, payload, traceparent) VALUES ($1, $2, $3, $4)`,
		uuid.New(), events.RoutingKeyPremiumUpdated, payload, "00-"+traceID+"-00f067aa0ba902b7-01"); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	deliveries := listen(t, url, events.RoutingKeyPremiumUpdated)
	publisher := rabbitmq.NewPublisher(url)
	t.Cleanup(publisher.Close)
	relay := outbox.NewRelay(repository.NewOutboxRepository(pool), publisher, "shared-secret")

	if n, err := relay.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("Drain = %d, %v; want 1, nil", n, err)
	}
	got, _ := receive(t, deliveries).Headers["traceparent"].(string)
	if !strings.Contains(got, traceID) {
		t.Errorf("traceparent = %q, want it to carry trace %s", got, traceID)
	}
}
