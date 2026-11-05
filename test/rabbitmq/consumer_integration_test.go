package rabbitmq_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/discordia-grupo01/events-contract/broker"

	"github.com/discordia-grupo01/billing/internal/rabbitmq"
)

func publishRaw(t *testing.T, url, routingKey, body string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if err := ch.PublishWithContext(context.Background(), broker.ExchangeName, routingKey, false, false,
		amqp.Publishing{ContentType: "application/json", Body: []byte(body)}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestConsumerAcksHandledAndRequeuesFailed(t *testing.T) {
	url := rabbitURL(t)
	queue := "billing.consumer-test-" + time.Now().Format("150405.000000")
	key := "billing.consumer_test." + time.Now().Format("150405.000000")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts atomic.Int32
	handled := make(chan string, 1)
	consumer := rabbitmq.NewConsumer(url, queue, []string{key}, func(_ context.Context, routingKey string, body []byte) error {
		if attempts.Add(1) == 1 {
			return errors.New("try again")
		}
		handled <- routingKey + ":" + string(body)
		return nil
	}, nil)
	done := make(chan struct{})
	go func() {
		consumer.Run(ctx)
		close(done)
	}()

	waitForQueue(t, url, queue)
	publishRaw(t, url, key, `{"a":1}`)

	select {
	case got := <-handled:
		if got != key+`:{"a":1}` {
			t.Errorf("got %q", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the event never arrived")
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want the failed one requeued once", attempts.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run() didn't stop")
	}
	deleteQueue(t, url, queue)
}

func waitForQueue(t *testing.T, url, queue string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	for range 50 {
		ch, err := conn.Channel()
		if err != nil {
			t.Fatalf("channel: %v", err)
		}
		_, err = ch.QueueDeclarePassive(queue, true, false, false, false, nil)
		if err == nil {
			_ = ch.Close()
			time.Sleep(200 * time.Millisecond)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the consumer never declared its queue")
}

func deleteQueue(t *testing.T, url, queue string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		return
	}
	defer conn.Close()
	if ch, err := conn.Channel(); err == nil {
		_, _ = ch.QueueDelete(queue, false, false, false)
	}
}
