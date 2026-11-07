package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/telemetry"
)

// maxErrorLength bounds last_error, so a huge broker error can't bloat the row.
const maxErrorLength = 1000

type OutboxRepository struct {
	pool *pgxpool.Pool
}

func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{pool: pool}
}

// PublishNext locks the next unpublished event and hands it to publish. If
// publish succeeds the event is marked published; if not, attempts and
// last_error are saved and publish's error is returned. Returns false when
// there is nothing to publish.
//
// Events that failed before go last, so one that keeps failing doesn't hold
// up the rest. Consumers use version, not arrival order, so reordering is
// safe. SKIP LOCKED lets several instances relay at the same time.
func (r *OutboxRepository) PublishNext(ctx context.Context, publish func(context.Context, model.OutboxEvent) error) (bool, error) {
	found := false
	var publishErr error
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var e model.OutboxEvent
		err := tx.QueryRow(ctx, `
			SELECT id, routing_key, payload, attempts, COALESCE(traceparent, '') FROM outbox_events
			WHERE published_at IS NULL
			ORDER BY attempts, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED`).Scan(&e.ID, &e.RoutingKey, &e.Payload, &e.Attempts, &e.Traceparent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true

		if publishErr = publish(ctx, e); publishErr != nil {
			msg := publishErr.Error()
			if len(msg) > maxErrorLength {
				msg = msg[:maxErrorLength]
			}
			_, err := tx.Exec(ctx,
				`UPDATE outbox_events SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, e.ID, msg)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at = now(), last_error = NULL WHERE id = $1`, e.ID)
		return err
	})
	if err != nil {
		return found, fmt.Errorf("repository: publish next outbox event: %w", err)
	}
	return found, publishErr
}

// insertOutbox must run inside the same transaction as the change the event
// describes, so a change is never committed without its event.
func insertOutbox(ctx context.Context, tx pgx.Tx, routingKey string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal %q: %w", routingKey, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, routing_key, payload, traceparent) VALUES ($1, $2, $3, NULLIF($4, ''))`,
		uuid.New(), routingKey, body, telemetry.Traceparent(ctx)); err != nil {
		return fmt.Errorf("outbox: insert %q: %w", routingKey, err)
	}
	return nil
}
