package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/discordia-grupo01/events-contract/account"
	"github.com/discordia-grupo01/events-contract/envelope"
)

const (
	changedPoints    = "points"
	changedBoosts    = "boosts"
	changedPurchases = "purchases"
)

func publishAccountChanged(ctx context.Context, tx pgx.Tx, userID uuid.UUID, resource string) error {
	return insertOutbox(ctx, tx, account.RoutingKeyChanged, account.Changed{
		Meta:     envelope.Meta{EventID: uuid.NewString(), OccurredAt: time.Now().UTC()},
		UserID:   userID.String(),
		Resource: resource,
	})
}
