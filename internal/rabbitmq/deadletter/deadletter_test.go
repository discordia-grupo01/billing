package deadletter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/discordia-grupo01/billing/internal/rabbitmq/deadletter"
)

type fakeAck struct {
	acks     int
	nacks    int
	requeued bool
}

func (f *fakeAck) Ack(uint64, bool) error { f.acks++; return nil }
func (f *fakeAck) Nack(_ uint64, _ bool, requeue bool) error {
	f.nacks++
	f.requeued = requeue
	return nil
}
func (f *fakeAck) Reject(uint64, bool) error { return nil }

type fakeSender struct {
	err  error
	sent []sentMessage
}

type sentMessage struct {
	queue string
	msg   amqp.Publishing
}

func (f *fakeSender) Send(_ context.Context, queue string, msg amqp.Publishing) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMessage{queue: queue, msg: msg})
	return nil
}

func fastPolicy() deadletter.Policy {
	return deadletter.Policy{MaxAttempts: 5, BaseDelay: time.Microsecond, MaxDelay: time.Millisecond}
}

func delivery(ack *fakeAck, routingKey, body string) amqp.Delivery {
	return amqp.Delivery{
		Acknowledger: ack,
		RoutingKey:   routingKey,
		Exchange:     "discordia.events",
		ContentType:  "application/json",
		Body:         []byte(body),
	}
}

func TestPolicyDelay_DoublesUntilTheCap(t *testing.T) {
	p := deadletter.DefaultPolicy()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, expected := range want {
		if got := p.Delay(i+1, 0.5); got != expected {
			t.Errorf("attempt %d: got %s, want %s", i+1, got, expected)
		}
	}
}

func TestPolicyDelay_JitterStaysWithinTwentyPercent(t *testing.T) {
	p := deadletter.DefaultPolicy()
	low, high := p.Delay(3, 0), p.Delay(3, 1)
	if low != 3200*time.Millisecond {
		t.Errorf("lowest delay: got %s, want 3.2s", low)
	}
	if high != 4800*time.Millisecond {
		t.Errorf("highest delay: got %s, want 4.8s", high)
	}
}

func TestSettle_AcknowledgesWhenProcessingSucceeded(t *testing.T) {
	sender := &fakeSender{}
	r := deadletter.NewWith("svc.queue", fastPolicy(), sender, nil)
	ack := &fakeAck{}

	r.Settle(context.Background(), delivery(ack, "servers.member_joined", `{"event_id":"e1"}`), false)

	if ack.acks != 1 || ack.nacks != 0 || len(sender.sent) != 0 {
		t.Fatalf("acks=%d nacks=%d sent=%d", ack.acks, ack.nacks, len(sender.sent))
	}
}

func TestSettle_RequeuesWhileAttemptsRemain(t *testing.T) {
	sender := &fakeSender{}
	r := deadletter.NewWith("svc.queue", fastPolicy(), sender, nil)

	for i := 1; i <= 4; i++ {
		ack := &fakeAck{}
		r.Settle(context.Background(), delivery(ack, "k", `{"event_id":"e1"}`), true)
		if ack.nacks != 1 || !ack.requeued || ack.acks != 0 {
			t.Fatalf("attempt %d: acks=%d nacks=%d requeued=%v", i, ack.acks, ack.nacks, ack.requeued)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatalf("nothing should reach the dead letter queue before the limit, got %d", len(sender.sent))
	}
}

func TestSettle_MovesToTheDeadLetterQueueOnTheLastAttempt(t *testing.T) {
	sender := &fakeSender{}
	r := deadletter.NewWith("svc.queue", fastPolicy(), sender, nil)
	body := `{"event_id":"evt-9","user_id":"u1"}`

	for i := 1; i <= 4; i++ {
		r.Settle(context.Background(), delivery(&fakeAck{}, "users.profile_updated", body), true)
	}
	last := &fakeAck{}
	r.Settle(context.Background(), delivery(last, "users.profile_updated", body), true)

	if last.acks != 1 || last.nacks != 0 {
		t.Fatalf("the original must be acknowledged once moved: acks=%d nacks=%d", last.acks, last.nacks)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected one dead letter, got %d", len(sender.sent))
	}
	got := sender.sent[0]
	if got.queue != "svc.queue" {
		t.Errorf("routing key = %q, want the source queue name", got.queue)
	}
	if string(got.msg.Body) != body {
		t.Errorf("body changed: %s", got.msg.Body)
	}
	if got.msg.DeliveryMode != amqp.Persistent {
		t.Errorf("dead letters must be persistent")
	}
	headers := map[string]any{
		"x-original-queue":       "svc.queue",
		"x-original-routing-key": "users.profile_updated",
		"x-original-exchange":    "discordia.events",
		"x-failure-reason":       deadletter.ReasonRetriesExhausted,
		"x-attempts":             int32(5),
		"x-event-id":             "evt-9",
	}
	for key, want := range headers {
		if got.msg.Headers[key] != want {
			t.Errorf("header %s = %v, want %v", key, got.msg.Headers[key], want)
		}
	}
	if _, ok := got.msg.Headers["x-failed-at"]; !ok {
		t.Errorf("missing x-failed-at header")
	}
}

func TestSettle_NeverAcknowledgesWhenTheDeadLetterQueueIsUnreachable(t *testing.T) {
	sender := &fakeSender{err: errors.New("broker down")}
	r := deadletter.NewWith("svc.queue", fastPolicy(), sender, nil)

	var last *fakeAck
	for i := 1; i <= 7; i++ {
		last = &fakeAck{}
		r.Settle(context.Background(), delivery(last, "k", `{"event_id":"e1"}`), true)
		if last.acks != 0 || last.nacks != 1 || !last.requeued {
			t.Fatalf("attempt %d: the event must stay in its queue: acks=%d nacks=%d requeued=%v", i, last.acks, last.nacks, last.requeued)
		}
	}
}

func TestSettle_ASuccessResetsTheCounter(t *testing.T) {
	sender := &fakeSender{}
	r := deadletter.NewWith("svc.queue", fastPolicy(), sender, nil)
	body := `{"event_id":"e1"}`

	for i := 0; i < 4; i++ {
		r.Settle(context.Background(), delivery(&fakeAck{}, "k", body), true)
	}
	r.Settle(context.Background(), delivery(&fakeAck{}, "k", body), false)
	for i := 0; i < 4; i++ {
		r.Settle(context.Background(), delivery(&fakeAck{}, "k", body), true)
	}

	if len(sender.sent) != 0 {
		t.Fatalf("the counter should have restarted after the success, got %d dead letters", len(sender.sent))
	}
}

func TestSettle_CountsEachMessageSeparately(t *testing.T) {
	sender := &fakeSender{}
	r := deadletter.NewWith("svc.queue", fastPolicy(), sender, nil)

	for i := 0; i < 4; i++ {
		r.Settle(context.Background(), delivery(&fakeAck{}, "k", `{"event_id":"a"}`), true)
		r.Settle(context.Background(), delivery(&fakeAck{}, "k", `{"event_id":"b"}`), true)
	}

	if len(sender.sent) != 0 {
		t.Fatalf("four failures of each message are still below the limit, got %d dead letters", len(sender.sent))
	}
}

func TestSettle_StopsWaitingWhenTheContextIsCancelled(t *testing.T) {
	r := deadletter.NewWith("svc.queue", deadletter.Policy{MaxAttempts: 5, BaseDelay: time.Hour, MaxDelay: time.Hour}, &fakeSender{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ack := &fakeAck{}

	done := make(chan struct{})
	go func() {
		r.Settle(ctx, delivery(ack, "k", `{"event_id":"e1"}`), true)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Settle kept waiting after the context was cancelled")
	}
	if ack.nacks != 1 || !ack.requeued {
		t.Fatalf("the event must go back to the queue: nacks=%d requeued=%v", ack.nacks, ack.requeued)
	}
}
