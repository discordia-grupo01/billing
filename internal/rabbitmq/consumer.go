package rabbitmq

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/discordia-grupo01/billing/internal/rabbitmq/deadletter"
	"github.com/discordia-grupo01/billing/internal/telemetry"
	"github.com/discordia-grupo01/events-contract/broker"
)

const (
	consumerConnectionName = "discordia-billing-events-consumer"
	reconnectMinDelay      = time.Second
	reconnectMaxDelay      = 30 * time.Second
	prefetch               = 10
)

// Handler processes one delivery. An error requeues it, so only return one for
// what can work on a retry.
type Handler func(ctx context.Context, routingKey string, body []byte) error

// Consumer reads a durable queue with manual acks and reconnects on any failure.
type Consumer struct {
	url         string
	queue       string
	routingKeys []string
	handle      Handler
	log         *slog.Logger
}

func NewConsumer(url, queue string, routingKeys []string, handle Handler, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{url: url, queue: queue, routingKeys: routingKeys, handle: handle, log: logger.With("queue", queue)}
}

// Run consumes until ctx ends.
func (c *Consumer) Run(ctx context.Context) {
	delay := reconnectMinDelay
	for ctx.Err() == nil {
		consumed, err := c.consumeOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if consumed {
			delay = reconnectMinDelay
		}
		c.log.Warn("consumer disconnected", "error", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// consumeOnce reports whether it got to process a delivery, which resets the backoff.
func (c *Consumer) consumeOnce(ctx context.Context) (bool, error) {
	props := amqp.NewConnectionProperties()
	props.SetClientConnectionName(consumerConnectionName)
	conn, err := amqp.DialConfig(c.url, amqp.Config{Dial: amqp.DefaultDial(dialTimeout), Properties: props})
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return false, fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return false, fmt.Errorf("set prefetch: %w", err)
	}
	if err := ch.ExchangeDeclare(broker.ExchangeName, broker.ExchangeType, true, false, false, false, nil); err != nil {
		return false, fmt.Errorf("declare exchange: %w", err)
	}
	if _, err := ch.QueueDeclare(c.queue, true, false, false, false, nil); err != nil {
		return false, fmt.Errorf("declare queue: %w", err)
	}
	for _, key := range c.routingKeys {
		if err := ch.QueueBind(c.queue, key, broker.ExchangeName, false, nil); err != nil {
			return false, fmt.Errorf("bind %s: %w", key, err)
		}
	}
	retrier, err := deadletter.New(conn, c.queue, deadletter.DefaultPolicy())
	if err != nil {
		return false, fmt.Errorf("dead letter queue: %w", err)
	}
	defer retrier.Close()

	deliveries, err := ch.Consume(c.queue, "", false, false, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consume: %w", err)
	}
	c.log.Info("consumer connected")

	consumed := false
	for {
		select {
		case <-ctx.Done():
			return consumed, ctx.Err()
		case d, open := <-deliveries:
			if !open {
				return consumed, fmt.Errorf("deliveries closed")
			}
			consumed = true
			c.process(ctx, retrier, d)
		}
	}
}

func (c *Consumer) process(ctx context.Context, retrier *deadletter.Retrier, d amqp.Delivery) {
	spanCtx, end := telemetry.StartConsumer(ctx, c.queue, d)
	err := c.handle(spanCtx, d.RoutingKey, d.Body)
	if err != nil {
		c.log.Error("event failed", "routing_key", d.RoutingKey, "error", err)
	}
	retrier.Settle(spanCtx, d, err != nil)
	end(err != nil)
}
