package deadletter_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/discordia-grupo01/billing/internal/rabbitmq/deadletter"
)

func TestRetrier_DeliversTheDeadLetterToARealBroker(t *testing.T) {
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL not set: skipping integration test")
	}
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("amqp.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("conn.Channel: %v", err)
	}
	queue := "billing.dlq_test_" + uuid.NewString()
	t.Cleanup(func() { ch.QueueDelete(queue+deadletter.QueueSuffix, false, false, false) })

	retrier, err := deadletter.New(conn, queue, fastPolicy())
	if err != nil {
		t.Fatalf("deadletter.New: %v", err)
	}
	t.Cleanup(func() { retrier.Close() })

	body := `{"event_id":"evt-integration"}`
	var last *fakeAck
	for i := 0; i < 5; i++ {
		last = &fakeAck{}
		retrier.Settle(context.Background(), delivery(last, "users.profile_updated", body), true)
	}
	if last.acks != 1 {
		t.Fatalf("the original must be acknowledged once the dead letter is confirmed: acks=%d", last.acks)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		msg, ok, err := ch.Get(queue+deadletter.QueueSuffix, true)
		if err != nil {
			t.Fatalf("ch.Get: %v", err)
		}
		if ok {
			if string(msg.Body) != body {
				t.Fatalf("body = %s", msg.Body)
			}
			if msg.Headers["x-original-queue"] != queue || msg.Headers["x-attempts"] != int32(5) {
				t.Fatalf("headers = %v", msg.Headers)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead letter never reached the dead letter queue")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
