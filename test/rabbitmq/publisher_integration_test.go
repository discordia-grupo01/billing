package rabbitmq_test

import (
	"context"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/discordia-grupo01/events-contract/broker"

	"github.com/discordia-grupo01/billing/internal/rabbitmq"
)

func rabbitURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL not set: skipping integration test")
	}
	return url
}

// listen binds an exclusive queue to routingKey and returns its deliveries.
func listen(t *testing.T, url, routingKey string) <-chan amqp.Delivery {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := ch.ExchangeDeclare(broker.ExchangeName, broker.ExchangeType, true, false, false, false, nil); err != nil {
		t.Fatalf("declare exchange: %v", err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatalf("declare queue: %v", err)
	}
	if err := ch.QueueBind(q.Name, routingKey, broker.ExchangeName, false, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	deliveries, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	return deliveries
}

func receive(t *testing.T, deliveries <-chan amqp.Delivery) amqp.Delivery {
	t.Helper()
	select {
	case d := <-deliveries:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
		return amqp.Delivery{}
	}
}

func TestPublishDeliversPersistentJSON(t *testing.T) {
	url := rabbitURL(t)
	deliveries := listen(t, url, "billing.test_event")
	p := rabbitmq.NewPublisher(url)
	t.Cleanup(p.Close)

	if err := p.Publish(context.Background(), "billing.test_event", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	d := receive(t, deliveries)
	if string(d.Body) != `{"n":1}` || d.ContentType != "application/json" || d.DeliveryMode != amqp.Persistent {
		t.Errorf("unexpected delivery: body %s, type %s, mode %d", d.Body, d.ContentType, d.DeliveryMode)
	}
}

func TestPublishReconnectsAfterClose(t *testing.T) {
	url := rabbitURL(t)
	deliveries := listen(t, url, "billing.test_event")
	p := rabbitmq.NewPublisher(url)
	t.Cleanup(p.Close)
	ctx := context.Background()

	if err := p.Publish(ctx, "billing.test_event", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	receive(t, deliveries)

	p.Close()
	if err := p.Publish(ctx, "billing.test_event", []byte(`{"n":2}`)); err != nil {
		t.Fatalf("Publish after Close: %v", err)
	}
	if d := receive(t, deliveries); string(d.Body) != `{"n":2}` {
		t.Errorf("body = %s", d.Body)
	}
}

func TestPublishWithCancelledContext(t *testing.T) {
	p := rabbitmq.NewPublisher(rabbitURL(t))
	t.Cleanup(p.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := p.Publish(ctx, "billing.test_event", []byte(`{}`)); err == nil {
		t.Error("expected an error with a cancelled context")
	}
	// The publisher must still work afterwards.
	if err := p.Publish(context.Background(), "billing.test_event", []byte(`{}`)); err != nil {
		t.Errorf("Publish after a failed one: %v", err)
	}
}
