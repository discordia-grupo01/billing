package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/events-contract/boosts"
	"github.com/discordia-grupo01/events-contract/envelope"

	"github.com/discordia-grupo01/billing/internal/model"
)

// BoostRepository owns server_boosts. It moves points with point_wallets and
// point_ledger in the same transaction as the boost and its event.
type BoostRepository struct {
	pool *pgxpool.Pool
}

func NewBoostRepository(pool *pgxpool.Pool) *BoostRepository {
	return &BoostRepository{pool: pool}
}

const boostColumns = `id, user_id, server_id, points, status, COALESCE(status_detail, ''), created_at, updated_at`

func scanBoost(row pgx.Row) (model.Boost, error) {
	var b model.Boost
	err := row.Scan(&b.ID, &b.UserID, &b.ServerID, &b.Points, &b.Status, &b.StatusDetail, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Boost{}, ErrNotFound
	}
	return b, err
}

// Create reserves the points and asks servers to apply the boost, all in one
// transaction. A key already used by this user returns that boost, with false,
// without charging again. Not enough points returns InsufficientPointsError and
// the whole transaction, including the new row, rolls back.
func (r *BoostRepository) Create(ctx context.Context, b model.Boost, idempotencyKey string) (model.Boost, bool, error) {
	var out model.Boost
	created := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		inserted, err := scanBoost(tx.QueryRow(ctx, `
			INSERT INTO server_boosts (id, user_id, server_id, points, idempotency_key)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''))
			ON CONFLICT (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
			RETURNING `+boostColumns, b.ID, b.UserID, b.ServerID, b.Points, idempotencyKey))
		if errors.Is(err, ErrNotFound) {
			out, err = scanBoost(tx.QueryRow(ctx,
				`SELECT `+boostColumns+` FROM server_boosts WHERE user_id = $1 AND idempotency_key = $2`,
				b.UserID, idempotencyKey))
			return err
		}
		if err != nil {
			return err
		}

		if err := ensureWallet(ctx, tx, b.UserID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE point_wallets SET balance = balance - $2, updated_at = now()
			WHERE user_id = $1 AND balance >= $2`, b.UserID, b.Points)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var balance int64
			if err := tx.QueryRow(ctx, `SELECT balance FROM point_wallets WHERE user_id = $1`, b.UserID).Scan(&balance); err != nil {
				return err
			}
			return model.InsufficientPointsError{Balance: balance, Cost: int64(b.Points)}
		}
		if _, err := insertLedger(ctx, tx, b.UserID, -int64(b.Points), model.LedgerReasonBoost, b.ID.String()); err != nil {
			return err
		}
		if err := insertBoostEvent(ctx, tx, boosts.RoutingKeyRequested, inserted); err != nil {
			return err
		}
		if err := publishAccountChanged(ctx, tx, b.UserID, changedBoosts); err != nil {
			return err
		}
		out, created = inserted, true
		return nil
	})
	if err != nil {
		return model.Boost{}, false, fmt.Errorf("repository: create boost: %w", err)
	}
	return out, created, nil
}

func (r *BoostRepository) Get(ctx context.Context, id uuid.UUID) (model.Boost, error) {
	return scanBoost(r.pool.QueryRow(ctx, `SELECT `+boostColumns+` FROM server_boosts WHERE id = $1`, id))
}

func (r *BoostRepository) ListForUser(ctx context.Context, userID uuid.UUID, limit int) ([]model.Boost, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+boostColumns+` FROM server_boosts
		WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: list boosts: %w", err)
	}
	defer rows.Close()
	out := make([]model.Boost, 0)
	for rows.Next() {
		b, err := scanBoost(rows)
		if err != nil {
			return nil, fmt.Errorf("repository: list boosts: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkApplied only moves a pending boost, so a repeated answer has no effect.
func (r *BoostRepository) MarkApplied(ctx context.Context, id uuid.UUID) (bool, error) {
	applied := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var userID uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE server_boosts SET status = 'active', updated_at = now()
			WHERE id = $1 AND status = 'pending'
			RETURNING user_id`, id).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		applied = true
		return publishAccountChanged(ctx, tx, userID, changedBoosts)
	})
	if err != nil {
		return false, fmt.Errorf("repository: mark boost applied: %w", err)
	}
	return applied, nil
}

// Reject is the saga's compensation: it fails a pending boost and refunds its
// points. Only a pending boost moves, and the ledger's UNIQUE(reason, ref_id)
// backs that up, so the points can never be returned twice.
func (r *BoostRepository) Reject(ctx context.Context, id uuid.UUID, detail string) (bool, error) {
	refunded := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		b, err := scanBoost(tx.QueryRow(ctx, `
			UPDATE server_boosts SET status = 'failed', status_detail = NULLIF($2, ''), updated_at = now()
			WHERE id = $1 AND status = 'pending'
			RETURNING `+boostColumns, id, detail))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		refunded = true
		if err := refund(ctx, tx, b); err != nil {
			return err
		}
		return publishAccountChanged(ctx, tx, b.UserID, changedBoosts)
	})
	if err != nil {
		return false, fmt.Errorf("repository: reject boost: %w", err)
	}
	return refunded, nil
}

// Remove withdraws an active boost of the user, without refund, and tells
// servers. Removing someone else's boost looks like it doesn't exist.
func (r *BoostRepository) Remove(ctx context.Context, userID, id uuid.UUID) (model.Boost, error) {
	var out model.Boost
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		b, err := scanBoost(tx.QueryRow(ctx, `
			UPDATE server_boosts SET status = 'removed', updated_at = now()
			WHERE id = $1 AND user_id = $2 AND status = 'active'
			RETURNING `+boostColumns, id, userID))
		if errors.Is(err, ErrNotFound) {
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM server_boosts WHERE id = $1 AND user_id = $2)`, id, userID,
			).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return model.ErrBoostNotActive
			}
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out = b
		if err := insertBoostEvent(ctx, tx, boosts.RoutingKeyRemoved, b); err != nil {
			return err
		}
		return publishAccountChanged(ctx, tx, b.UserID, changedBoosts)
	})
	if err != nil {
		return model.Boost{}, fmt.Errorf("repository: remove boost: %w", err)
	}
	return out, nil
}

// RemoveForServer marks the active boosts of a deleted server as removed,
// without refund. servers already knows, so there is no event.
func (r *BoostRepository) RemoveForServer(ctx context.Context, serverID uuid.UUID) (int, error) {
	count := 0
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE server_boosts SET status = 'removed', status_detail = $2, updated_at = now()
			WHERE server_id = $1 AND status = 'active'
			RETURNING user_id`, serverID, model.BoostDetailServerDeleted)
		if err != nil {
			return err
		}
		var owners []uuid.UUID
		for rows.Next() {
			var userID uuid.UUID
			if err := rows.Scan(&userID); err != nil {
				rows.Close()
				return err
			}
			owners = append(owners, userID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		for _, userID := range owners {
			if seen[userID] {
				continue
			}
			seen[userID] = true
			if err := publishAccountChanged(ctx, tx, userID, changedBoosts); err != nil {
				return err
			}
		}
		count = len(owners)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("repository: remove boosts of server: %w", err)
	}
	return count, nil
}

// RepublishPending asks again about pending boosts last requested before the
// given time: RabbitMQ drops what is published while the queue doesn't exist.
func (r *BoostRepository) RepublishPending(ctx context.Context, before time.Time, limit int) (int, error) {
	count := 0
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		pending, err := lockPending(ctx, tx, `last_requested_at < $1`, before, limit)
		if err != nil {
			return err
		}
		for _, b := range pending {
			if err := insertBoostEvent(ctx, tx, boosts.RoutingKeyRequested, b); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE server_boosts SET last_requested_at = now() WHERE id = $1`, b.ID); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("repository: republish boosts: %w", err)
	}
	return count, nil
}

// CancelExpired gives up on boosts created before the given time: it refunds
// them and tells servers, which undoes the boost if it did apply it.
func (r *BoostRepository) CancelExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	count := 0
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		pending, err := lockPending(ctx, tx, `created_at < $1`, before, limit)
		if err != nil {
			return err
		}
		for _, b := range pending {
			if _, err := tx.Exec(ctx, `
				UPDATE server_boosts SET status = 'failed', status_detail = $2, updated_at = now()
				WHERE id = $1`, b.ID, model.BoostDetailNoResponse); err != nil {
				return err
			}
			if err := refund(ctx, tx, b); err != nil {
				return err
			}
			if err := insertBoostEvent(ctx, tx, boosts.RoutingKeyCancelled, b); err != nil {
				return err
			}
			if err := publishAccountChanged(ctx, tx, b.UserID, changedBoosts); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("repository: cancel expired boosts: %w", err)
	}
	return count, nil
}

func lockPending(ctx context.Context, tx pgx.Tx, condition string, before time.Time, limit int) ([]model.Boost, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+boostColumns+` FROM server_boosts
		WHERE status = 'pending' AND `+condition+`
		ORDER BY created_at LIMIT $2
		FOR UPDATE SKIP LOCKED`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Boost
	for rows.Next() {
		b, err := scanBoost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func refund(ctx context.Context, tx pgx.Tx, b model.Boost) error {
	inserted, err := insertLedger(ctx, tx, b.UserID, int64(b.Points), model.LedgerReasonBoostRefund, b.ID.String())
	if err != nil || !inserted {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE point_wallets SET balance = balance + $2, updated_at = now() WHERE user_id = $1`,
		b.UserID, b.Points)
	return err
}

// insertBoostEvent writes the event unsigned; the relay signs it when publishing.
func insertBoostEvent(ctx context.Context, tx pgx.Tx, routingKey string, b model.Boost) error {
	return insertOutbox(ctx, tx, routingKey, boosts.SagaEvent{
		Meta:     envelope.Meta{EventID: uuid.NewString(), OccurredAt: time.Now().UTC()},
		BoostID:  b.ID.String(),
		ServerID: b.ServerID.String(),
		UserID:   b.UserID.String(),
	})
}
