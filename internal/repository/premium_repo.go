package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
	events "github.com/discordia-grupo01/events-contract/billing"
	"github.com/discordia-grupo01/events-contract/envelope"
)

var ErrLiveSubscriptionExists = model.ErrLiveSubscriptionExists

const liveSubscriptionIndex = "uq_subscriptions_live_per_user"

// PremiumRepository owns subscriptions, subscription_charges and
// premium_status. Every change to premium_status bumps its version and writes
// a billing.premium_updated event in the same transaction.
//
// Lock order is always premium_status first, then subscriptions/charges, so
// a webhook and the reconciliation job never deadlock on the same user.
type PremiumRepository struct {
	pool *pgxpool.Pool
}

func NewPremiumRepository(pool *pgxpool.Pool) *PremiumRepository {
	return &PremiumRepository{pool: pool}
}

const subscriptionColumns = `mp_preapproval_id, user_id, mp_status, amount_cents, currency_id, created_at, updated_at`

func scanSubscription(row pgx.Row) (model.SubscriptionRecord, error) {
	var s model.SubscriptionRecord
	err := row.Scan(&s.MPPreapprovalID, &s.UserID, &s.MPStatus, &s.AmountCents, &s.CurrencyID, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.SubscriptionRecord{}, ErrNotFound
	}
	return s, err
}

func isLiveSubscriptionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == liveSubscriptionIndex
}

func (r *PremiumRepository) CreateSubscription(ctx context.Context, s model.SubscriptionRecord) (model.SubscriptionRecord, error) {
	created, err := scanSubscription(r.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (mp_preapproval_id, user_id, mp_status, amount_cents, currency_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+subscriptionColumns,
		s.MPPreapprovalID, s.UserID, s.MPStatus, s.AmountCents, s.CurrencyID))
	if isLiveSubscriptionViolation(err) {
		return model.SubscriptionRecord{}, ErrLiveSubscriptionExists
	}
	if err != nil {
		return model.SubscriptionRecord{}, fmt.Errorf("repository: create subscription: %w", err)
	}
	return created, nil
}

func (r *PremiumRepository) GetSubscription(ctx context.Context, preapprovalID string) (model.SubscriptionRecord, error) {
	return scanSubscription(r.pool.QueryRow(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions WHERE mp_preapproval_id = $1`, preapprovalID))
}

func (r *PremiumRepository) LatestSubscription(ctx context.Context, userID uuid.UUID) (model.SubscriptionRecord, error) {
	return scanSubscription(r.pool.QueryRow(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, userID))
}

// ListSubscriptionsToReconcile returns the subscriptions that may still
// change on MP's side, oldest-checked first. A cancelled one counts while the
// period it paid for is still running, because that payment can be refunded.
func (r *PremiumRepository) ListSubscriptionsToReconcile(ctx context.Context, limit int) ([]model.SubscriptionRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+subscriptionColumns+` FROM subscriptions s
		WHERE mp_status IN ('pending', 'authorized', 'paused')
		   OR (mp_status = 'cancelled' AND EXISTS (
				SELECT 1 FROM subscription_charges sc
				JOIN premium_status ps ON ps.user_id = s.user_id
				WHERE sc.mp_preapproval_id = s.mp_preapproval_id
				  AND sc.applied_at IS NOT NULL AND sc.revoked_at IS NULL
				  AND ps.premium_until > now()))
		ORDER BY updated_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: list subscriptions to reconcile: %w", err)
	}
	defer rows.Close()
	var out []model.SubscriptionRecord
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("repository: list subscriptions to reconcile: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SyncSubscriptionStatus stores the status MP reports for a preapproval.
// renews is recomputed from all the user's subscriptions, so syncing an old
// cancelled one never turns off the renewal of the current one. Returns
// whether premium_status changed (and an event was written).
func (r *PremiumRepository) SyncSubscriptionStatus(ctx context.Context, preapprovalID, mpStatus string) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		userID, err := subscriptionOwner(ctx, tx, preapprovalID)
		if err != nil {
			return err
		}
		p, err := lockPremium(ctx, tx, userID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE subscriptions SET mp_status = $2, updated_at = now()
			WHERE mp_preapproval_id = $1`, preapprovalID, mpStatus); err != nil {
			if isLiveSubscriptionViolation(err) {
				return ErrLiveSubscriptionExists
			}
			return err
		}

		var renews bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1 AND mp_status = 'authorized')`,
			userID).Scan(&renews); err != nil {
			return err
		}
		if renews == p.Renews {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE premium_status SET renews = $2 WHERE user_id = $1`, userID, renews); err != nil {
			return err
		}
		changed = true
		return emitPremiumUpdated(ctx, tx, userID)
	})
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLiveSubscriptionExists) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("repository: sync subscription status: %w", err)
	}
	return changed, nil
}

// ApplyCharge stores the latest state of a monthly charge. If MP collected
// it and it was never applied, premium_until moves one month forward from
// max(premium_until, debit_date), so a late retry reactivates from the day it
// was paid. Returns whether Premium was extended.
func (r *PremiumRepository) ApplyCharge(ctx context.Context, c model.Charge) (bool, error) {
	extended := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		userID, err := subscriptionOwner(ctx, tx, c.PreapprovalID)
		if err != nil {
			return err
		}
		if _, err := lockPremium(ctx, tx, userID); err != nil {
			return err
		}

		var appliedAt *time.Time
		var debitDate *time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO subscription_charges AS sc
				(mp_authorized_payment_id, mp_preapproval_id, mp_payment_id, status, payment_status,
				 amount_cents, debit_date, retry_attempt)
			VALUES ($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''), $6, $7, $8)
			ON CONFLICT (mp_authorized_payment_id) DO UPDATE SET
				mp_payment_id  = COALESCE(EXCLUDED.mp_payment_id, sc.mp_payment_id),
				status         = EXCLUDED.status,
				payment_status = COALESCE(EXCLUDED.payment_status, sc.payment_status),
				amount_cents   = EXCLUDED.amount_cents,
				debit_date     = COALESCE(EXCLUDED.debit_date, sc.debit_date),
				retry_attempt  = EXCLUDED.retry_attempt,
				updated_at     = now()
			RETURNING applied_at, debit_date`,
			c.AuthorizedPaymentID, c.PreapprovalID, c.PaymentID, c.Status, c.PaymentStatus,
			c.AmountCents, c.DebitDate, c.RetryAttempt).Scan(&appliedAt, &debitDate); err != nil {
			return err
		}
		if appliedAt != nil || c.PaymentStatus != model.PaymentStatusApproved {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE premium_status
			SET premium_until = GREATEST(COALESCE(premium_until, $2), $2) + interval '1 month'
			WHERE user_id = $1`, userID, coalesceNow(debitDate)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE subscription_charges SET applied_at = now() WHERE mp_authorized_payment_id = $1`,
			c.AuthorizedPaymentID); err != nil {
			return err
		}
		extended = true
		return emitPremiumUpdated(ctx, tx, userID)
	})
	if errors.Is(err, ErrNotFound) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("repository: apply charge: %w", err)
	}
	return extended, nil
}

type Revocation = model.Revocation

// RevokeCharge handles a refunded or charged-back monthly payment: Premium
// ends now and stops renewing. It only revokes once per payment, and only if
// that charge had extended Premium. Returns false if nothing was revoked.
func (r *PremiumRepository) RevokeCharge(ctx context.Context, paymentID, paymentStatus string) (Revocation, bool, error) {
	var rev Revocation
	revoked := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT s.user_id, s.mp_preapproval_id FROM subscription_charges sc
			JOIN subscriptions s USING (mp_preapproval_id)
			WHERE sc.mp_payment_id = $1`, paymentID).Scan(&rev.UserID, &rev.PreapprovalID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := lockPremium(ctx, tx, rev.UserID); err != nil {
			return err
		}

		var appliedAt, revokedAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT applied_at, revoked_at FROM subscription_charges WHERE mp_payment_id = $1 FOR UPDATE`,
			paymentID).Scan(&appliedAt, &revokedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE subscription_charges SET payment_status = $2, updated_at = now() WHERE mp_payment_id = $1`,
			paymentID, paymentStatus); err != nil {
			return err
		}
		if appliedAt == nil || revokedAt != nil {
			return nil
		}

		if _, err := tx.Exec(ctx, `
			UPDATE premium_status SET premium_until = LEAST(premium_until, now()), renews = false
			WHERE user_id = $1`, rev.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE subscription_charges SET revoked_at = now() WHERE mp_payment_id = $1`, paymentID); err != nil {
			return err
		}
		revoked = true
		return emitPremiumUpdated(ctx, tx, rev.UserID)
	})
	if errors.Is(err, ErrNotFound) {
		return Revocation{}, false, err
	}
	if err != nil {
		return Revocation{}, false, fmt.Errorf("repository: revoke charge: %w", err)
	}
	return rev, revoked, nil
}

// ExpireDue emits active=false for users whose premium_until passed and whose
// last event still said active. SKIP LOCKED lets two job instances split the
// work. Returns how many users expired.
func (r *PremiumRepository) ExpireDue(ctx context.Context, limit int) (int, error) {
	expired := 0
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT user_id FROM premium_status
			WHERE active_emitted AND premium_until <= now()
			ORDER BY premium_until LIMIT $1
			FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		userIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range userIDs {
			if err := emitPremiumUpdated(ctx, tx, id); err != nil {
				return err
			}
		}
		expired = len(userIDs)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("repository: expire due: %w", err)
	}
	return expired, nil
}

// RepublishAll emits a new billing.premium_updated with the current state of
// every user in premium_status, for consumers whose queue didn't exist when the
// original events went out. It bumps each version, so a consumer that already
// has the user applies it too, with the same result. Each user has its own
// transaction: after a failure, running it again is safe. Returns how many
// events were written.
func (r *PremiumRepository) RepublishAll(ctx context.Context) (int, error) {
	rows, err := r.pool.Query(ctx, `SELECT user_id FROM premium_status ORDER BY user_id`)
	if err != nil {
		return 0, fmt.Errorf("repository: list premium users: %w", err)
	}
	userIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("repository: list premium users: %w", err)
	}
	for i, id := range userIDs {
		err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			return emitPremiumUpdated(ctx, tx, id)
		})
		if err != nil {
			return i, fmt.Errorf("repository: republish premium of %s: %w", id, err)
		}
	}
	return len(userIDs), nil
}

// EndPremiumPeriod moves premium_until a second into the past, as if the
// paid period had just ended; the expiry job then emits active=false. Only
// the dev simulator calls it. Returns false if the user had no active
// Premium.
func (r *PremiumRepository) EndPremiumPeriod(ctx context.Context, userID uuid.UUID) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE premium_status SET premium_until = now() - interval '1 second', updated_at = now()
		WHERE user_id = $1 AND premium_until > now()`, userID)
	if err != nil {
		return false, fmt.Errorf("repository: end premium period: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetPremium returns a zero Premium (not an error) for users that never
// subscribed.
func (r *PremiumRepository) GetPremium(ctx context.Context, userID uuid.UUID) (model.Premium, error) {
	p, err := scanPremium(r.pool.QueryRow(ctx,
		`SELECT `+premiumColumns+` FROM premium_status WHERE user_id = $1`, userID))
	if errors.Is(err, ErrNotFound) {
		return model.Premium{UserID: userID}, nil
	}
	if err != nil {
		return model.Premium{}, fmt.Errorf("repository: get premium: %w", err)
	}
	return p, nil
}

// HasAppliedCharge tells "processing the first payment" apart from a
// subscription that already gave Premium.
func (r *PremiumRepository) HasAppliedCharge(ctx context.Context, preapprovalID string) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM subscription_charges WHERE mp_preapproval_id = $1 AND applied_at IS NOT NULL)`,
		preapprovalID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("repository: has applied charge: %w", err)
	}
	return ok, nil
}

const premiumColumns = `user_id, premium_until, renews, version, active_emitted, updated_at`

func scanPremium(row pgx.Row) (model.Premium, error) {
	var p model.Premium
	err := row.Scan(&p.UserID, &p.PremiumUntil, &p.Renews, &p.Version, &p.ActiveEmitted, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Premium{}, ErrNotFound
	}
	return p, err
}

func subscriptionOwner(ctx context.Context, tx pgx.Tx, preapprovalID string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT user_id FROM subscriptions WHERE mp_preapproval_id = $1`, preapprovalID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return userID, err
}

// lockPremium creates the user's row if needed and locks it until the
// transaction ends.
func lockPremium(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (model.Premium, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO premium_status (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return model.Premium{}, err
	}
	return scanPremium(tx.QueryRow(ctx,
		`SELECT `+premiumColumns+` FROM premium_status WHERE user_id = $1 FOR UPDATE`, userID))
}

// emitPremiumUpdated bumps the version and writes a snapshot of the row as
// it is now. active_emitted remembers what the event said, for ExpireDue.
func emitPremiumUpdated(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var evt events.PremiumUpdated
	var occurredAt time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE premium_status
		SET version = version + 1,
		    active_emitted = COALESCE(premium_until > now(), false),
		    updated_at = now()
		WHERE user_id = $1
		RETURNING premium_until, renews, version, active_emitted, now()`, userID).
		Scan(&evt.PremiumUntil, &evt.Renews, &evt.Version, &evt.Active, &occurredAt); err != nil {
		return err
	}
	evt.Meta = envelope.Meta{EventID: uuid.NewString(), OccurredAt: occurredAt}
	evt.UserID = userID.String()
	return insertOutbox(ctx, tx, events.RoutingKeyPremiumUpdated, evt)
}

func coalesceNow(t *time.Time) time.Time {
	if t == nil {
		return time.Now()
	}
	return *t
}
