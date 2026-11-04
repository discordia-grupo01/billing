package repository_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/repository"
)

func insertEvent(t *testing.T, pool *pgxpool.Pool, key string, age string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO outbox_events (id, routing_key, payload, created_at)
		VALUES ($1, $2, '{"n":1}', now() - $3::interval)`, id, key, age); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

type outboxRow struct {
	published bool
	attempts  int
	lastError *string
}

func readEvent(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) outboxRow {
	t.Helper()
	var r outboxRow
	if err := pool.QueryRow(context.Background(),
		`SELECT published_at IS NOT NULL, attempts, last_error FROM outbox_events WHERE id = $1`, id).
		Scan(&r.published, &r.attempts, &r.lastError); err != nil {
		t.Fatalf("read event: %v", err)
	}
	return r
}

func TestPublishNextPublishesOldestFirst(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewOutboxRepository(pool)
	ctx := context.Background()
	older := insertEvent(t, pool, "billing.a", "2 minutes")
	newer := insertEvent(t, pool, "billing.b", "1 minute")

	var got []model.OutboxEvent
	publish := func(_ context.Context, e model.OutboxEvent) error {
		got = append(got, e)
		return nil
	}
	for range 3 {
		if _, err := repo.PublishNext(ctx, publish); err != nil {
			t.Fatalf("PublishNext: %v", err)
		}
	}

	if len(got) != 2 || got[0].ID != older || got[1].ID != newer {
		t.Fatalf("published %+v, want older then newer", got)
	}
	if got[0].RoutingKey != "billing.a" || !strings.Contains(string(got[0].Payload), `"n"`) {
		t.Errorf("unexpected event: %+v", got[0])
	}
	if !readEvent(t, pool, older).published || !readEvent(t, pool, newer).published {
		t.Error("both events should be marked published")
	}
}

func TestPublishNextWithNothingPending(t *testing.T) {
	repo := repository.NewOutboxRepository(newTestPool(t))
	found, err := repo.PublishNext(context.Background(), func(context.Context, model.OutboxEvent) error {
		t.Error("publish must not be called")
		return nil
	})
	if found || err != nil {
		t.Errorf("PublishNext = %v, %v; want false, nil", found, err)
	}
}

func TestPublishNextRecordsFailuresAndMovesOn(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewOutboxRepository(pool)
	ctx := context.Background()
	failing := insertEvent(t, pool, "billing.a", "2 minutes")
	other := insertEvent(t, pool, "billing.b", "1 minute")
	boom := errors.New(strings.Repeat("x", 5000))

	found, err := repo.PublishNext(ctx, func(context.Context, model.OutboxEvent) error { return boom })
	if !found || !errors.Is(err, boom) {
		t.Fatalf("PublishNext = %v, %v; want true, boom", found, err)
	}
	row := readEvent(t, pool, failing)
	if row.published || row.attempts != 1 || row.lastError == nil || len(*row.lastError) != 1000 {
		t.Errorf("failure not recorded (or last_error not truncated): published %v attempts %d", row.published, row.attempts)
	}

	// The failed event goes last, so the next call picks the other one.
	var next uuid.UUID
	if _, err := repo.PublishNext(ctx, func(_ context.Context, e model.OutboxEvent) error {
		next = e.ID
		return nil
	}); err != nil {
		t.Fatalf("PublishNext: %v", err)
	}
	if next != other {
		t.Errorf("published %s, want the event that never failed (%s)", next, other)
	}

	// A later success clears last_error.
	var retried model.OutboxEvent
	if _, err := repo.PublishNext(ctx, func(_ context.Context, e model.OutboxEvent) error {
		retried = e
		return nil
	}); err != nil {
		t.Fatalf("PublishNext: %v", err)
	}
	row = readEvent(t, pool, failing)
	if retried.ID != failing || retried.Attempts != 1 || !row.published || row.lastError != nil {
		t.Errorf("retry: event %+v, row %+v", retried, row)
	}
}

func TestPublishNextDBError(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewOutboxRepository(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.PublishNext(ctx, func(context.Context, model.OutboxEvent) error { return nil }); err == nil {
		t.Error("expected an error with a cancelled context")
	}
}
