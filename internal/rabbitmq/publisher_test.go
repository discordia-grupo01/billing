package rabbitmq

import (
	"context"
	"testing"
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
