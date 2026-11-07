// Package rabbitmq publishes billing's events to the discordia.events
// exchange.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/discordia-grupo01/events-contract/broker"

	"github.com/discordia-grupo01/billing/internal/telemetry"
)

const (
	dialTimeout = 5 * time.Second
	// Shown in the RabbitMQ management UI.
	connectionName = "discordia-billing-events-publisher"
)

var ErrNacked = errors.New("rabbitmq: broker did not accept the message")

// Publisher uses publisher confirms: Publish returns only after the broker
// took the message, so the outbox never marks as published an event that was
// lost on the way. It connects lazily and reconnects after any failure.
type Publisher struct {
	url string

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

func NewPublisher(url string) *Publisher {
	return &Publisher{url: url}
}

func (p *Publisher) Publish(ctx context.Context, routingKey string, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureChannel(); err != nil {
		return err
	}
	confirm, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, broker.ExchangeName, routingKey, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
			Headers:      telemetry.InjectAMQP(ctx, nil),
		})
	if err != nil {
		p.closeLocked()
		return fmt.Errorf("rabbitmq: publish %s: %w", routingKey, err)
	}
	acked, err := confirm.WaitContext(ctx)
	if err != nil {
		// The confirm may still arrive on this channel; a fresh one avoids
		// mixing it up with the next message.
		p.closeLocked()
		return fmt.Errorf("rabbitmq: wait confirm %s: %w", routingKey, err)
	}
	if !acked {
		return fmt.Errorf("%w: %s", ErrNacked, routingKey)
	}
	return nil
}

func (p *Publisher) ensureChannel() error {
	if p.ch != nil && !p.ch.IsClosed() {
		return nil
	}
	p.closeLocked()

	props := amqp.NewConnectionProperties()
	props.SetClientConnectionName(connectionName)
	conn, err := amqp.DialConfig(p.url, amqp.Config{Dial: amqp.DefaultDial(dialTimeout), Properties: props})
	if err != nil {
		return fmt.Errorf("rabbitmq: connect: %w", err)
	}
	ch, err := conn.Channel()
	if err == nil {
		err = ch.Confirm(false)
	}
	if err == nil {
		err = ch.ExchangeDeclare(broker.ExchangeName, broker.ExchangeType, true, false, false, false, nil)
	}
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("rabbitmq: open channel: %w", err)
	}
	p.conn, p.ch = conn, ch
	return nil
}

// Close closes the connection. Publish reconnects if called again.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
}

func (p *Publisher) closeLocked() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}
