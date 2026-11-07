package deadletter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeName           = "discordia.dlx"
	QueueSuffix            = ".dlq"
	ReasonRetriesExhausted = "retries_exhausted"

	maxTracked = 1024
)

type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64
}

func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts: 5,
		BaseDelay:   time.Second,
		MaxDelay:    30 * time.Second,
		Jitter:      0.2,
	}
}

func (p Policy) Delay(attempt int, random float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := p.BaseDelay
	for i := 1; i < attempt && delay < p.MaxDelay; i++ {
		delay *= 2
	}
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	spread := 1 + p.Jitter*(2*random-1)
	return time.Duration(float64(delay) * spread)
}

type Sender interface {
	Send(ctx context.Context, queue string, msg amqp.Publishing) error
}

type Retrier struct {
	queue    string
	policy   Policy
	sender   Sender
	closer   func() error
	attempts map[string]int
}

func New(conn *amqp.Connection, queue string, policy Policy) (*Retrier, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := Declare(ch, queue); err != nil {
		_ = ch.Close()
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return NewWith(queue, policy, channelSender{ch: ch}, ch.Close), nil
}

func NewWith(queue string, policy Policy, sender Sender, closer func() error) *Retrier {
	return &Retrier{
		queue:    queue,
		policy:   policy,
		sender:   sender,
		closer:   closer,
		attempts: make(map[string]int),
	}
}

func Declare(ch *amqp.Channel, queue string) error {
	if err := ch.ExchangeDeclare(ExchangeName, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	dlq, err := ch.QueueDeclare(queue+QueueSuffix, true, false, false, false, nil)
	if err != nil {
		return err
	}
	return ch.QueueBind(dlq.Name, queue, ExchangeName, false, nil)
}

func (r *Retrier) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer()
}

func (r *Retrier) Settle(ctx context.Context, d amqp.Delivery, retry bool) {
	key := identity(d)
	if !retry {
		delete(r.attempts, key)
		_ = d.Ack(false)
		return
	}

	attempt := r.attempts[key] + 1
	if attempt >= r.policy.MaxAttempts {
		err := r.sender.Send(ctx, r.queue, r.deadLetter(d, attempt))
		if err == nil {
			delete(r.attempts, key)
			log.Printf("deadletter: queue=%s routing_key=%s event_id=%s moved to %s%s after %d attempts", r.queue, d.RoutingKey, eventID(d.Body), r.queue, QueueSuffix, attempt)
			_ = d.Ack(false)
			return
		}
		log.Printf("deadletter: queue=%s routing_key=%s event_id=%s could not be moved to %s%s, retrying: %v", r.queue, d.RoutingKey, eventID(d.Body), r.queue, QueueSuffix, err)
	}

	if len(r.attempts) >= maxTracked {
		r.attempts = make(map[string]int)
	}
	r.attempts[key] = attempt
	delay := r.policy.Delay(attempt, rand.Float64())
	log.Printf("deadletter: queue=%s routing_key=%s event_id=%s attempt %d failed, retrying in %s", r.queue, d.RoutingKey, eventID(d.Body), attempt, delay)
	wait(ctx, delay)
	_ = d.Nack(false, true)
}

func (r *Retrier) deadLetter(d amqp.Delivery, attempts int) amqp.Publishing {
	headers := amqp.Table{
		"x-original-queue":       r.queue,
		"x-original-exchange":    d.Exchange,
		"x-original-routing-key": d.RoutingKey,
		"x-failure-reason":       ReasonRetriesExhausted,
		"x-attempts":             int32(attempts),
		"x-failed-at":            time.Now().UTC().Format(time.RFC3339),
	}
	if id := eventID(d.Body); id != "" {
		headers["x-event-id"] = id
	}
	return amqp.Publishing{
		ContentType:  d.ContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    d.MessageId,
		Timestamp:    time.Now(),
		Headers:      headers,
		Body:         d.Body,
	}
}

type channelSender struct {
	ch *amqp.Channel
}

func (s channelSender) Send(ctx context.Context, queue string, msg amqp.Publishing) error {
	confirmation, err := s.ch.PublishWithDeferredConfirmWithContext(ctx, ExchangeName, queue, false, false, msg)
	if err != nil {
		return err
	}
	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !acked {
		return errors.New("dead letter publish was not confirmed by the broker")
	}
	return nil
}

func identity(d amqp.Delivery) string {
	sum := sha256.New()
	sum.Write([]byte(d.RoutingKey))
	sum.Write([]byte{0})
	sum.Write(d.Body)
	return hex.EncodeToString(sum.Sum(nil))
}

func eventID(body []byte) string {
	var meta struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return ""
	}
	return meta.EventID
}

func wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
