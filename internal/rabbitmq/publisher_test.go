package rabbitmq

import (
	"context"
	"testing"
	"time"
)

func TestPublishFailsWhenBrokerIsUnreachable(t *testing.T) {
	p := NewPublisher("amqp://guest:guest@127.0.0.1:1/")
	defer p.Close()

	if err := p.Publish(context.Background(), "billing.test_event", []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
	if p.conn != nil || p.ch != nil {
		t.Error("a failed connect must leave no connection behind")
	}
}

func TestConsumerStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	consumer := NewConsumer("amqp://guest:guest@127.0.0.1:1/", "q", []string{"k"}, func(context.Context, string, []byte) error { return nil }, nil)
	go func() {
		consumer.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run() didn't stop")
	}
}
