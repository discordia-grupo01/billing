package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// insertOutbox must run inside the same transaction as the change the event
// describes, so a change is never committed without its event.
func insertOutbox(ctx context.Context, tx pgx.Tx, routingKey string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal %q: %w", routingKey, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, routing_key, payload) VALUES ($1, $2, $3)`,
		uuid.New(), routingKey, body); err != nil {
		return fmt.Errorf("outbox: insert %q: %w", routingKey, err)
	}
	return nil
}
