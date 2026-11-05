package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
)

// CheckoutRequestRepository owns checkout_requests.
type CheckoutRequestRepository struct {
	pool *pgxpool.Pool
}

func NewCheckoutRequestRepository(pool *pgxpool.Pool) *CheckoutRequestRepository {
	return &CheckoutRequestRepository{pool: pool}
}

// Reserve takes the key if it is new, or if an unfinished request holds it and
// was locked before staleBefore, which means that process died.
func (r *CheckoutRequestRepository) Reserve(ctx context.Context, k model.CheckoutKey, fingerprint string, staleBefore time.Time) (model.CheckoutReservation, error) {
	var acquired string
	err := r.pool.QueryRow(ctx, `
		INSERT INTO checkout_requests AS cr (user_id, scope, idempotency_key, fingerprint)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, scope, idempotency_key) DO UPDATE
			SET fingerprint = EXCLUDED.fingerprint, locked_at = now()
			WHERE cr.checkout_url IS NULL AND cr.locked_at < $5
		RETURNING fingerprint`, k.UserID, k.Scope, k.Key, fingerprint, staleBefore).Scan(&acquired)
	if err == nil {
		return model.CheckoutReservation{Acquired: true, Fingerprint: acquired}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.CheckoutReservation{}, fmt.Errorf("repository: reserve checkout: %w", err)
	}

	var res model.CheckoutReservation
	var purchaseID *uuid.UUID
	err = r.pool.QueryRow(ctx, `
		SELECT fingerprint, purchase_id, COALESCE(checkout_url, '') FROM checkout_requests
		WHERE user_id = $1 AND scope = $2 AND idempotency_key = $3`, k.UserID, k.Scope, k.Key).
		Scan(&res.Fingerprint, &purchaseID, &res.CheckoutURL)
	if errors.Is(err, pgx.ErrNoRows) {
		// The other request failed and released the key just now.
		return model.CheckoutReservation{Fingerprint: fingerprint}, nil
	}
	if err != nil {
		return model.CheckoutReservation{}, fmt.Errorf("repository: read checkout: %w", err)
	}
	if purchaseID != nil {
		res.PurchaseID = *purchaseID
	}
	return res, nil
}

// Complete stores the result, so a retry with the same key gets it back.
func (r *CheckoutRequestRepository) Complete(ctx context.Context, k model.CheckoutKey, purchaseID uuid.UUID, checkoutURL string) error {
	var purchase *uuid.UUID
	if purchaseID != uuid.Nil {
		purchase = &purchaseID
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE checkout_requests SET purchase_id = $4, checkout_url = $5
		WHERE user_id = $1 AND scope = $2 AND idempotency_key = $3`,
		k.UserID, k.Scope, k.Key, purchase, checkoutURL); err != nil {
		return fmt.Errorf("repository: complete checkout: %w", err)
	}
	return nil
}

// Release frees the key of a failed checkout, so it can be retried.
func (r *CheckoutRequestRepository) Release(ctx context.Context, k model.CheckoutKey) error {
	if _, err := r.pool.Exec(ctx, `
		DELETE FROM checkout_requests
		WHERE user_id = $1 AND scope = $2 AND idempotency_key = $3 AND checkout_url IS NULL`,
		k.UserID, k.Scope, k.Key); err != nil {
		return fmt.Errorf("repository: release checkout: %w", err)
	}
	return nil
}

func (r *CheckoutRequestRepository) DeleteOlderThan(ctx context.Context, before time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM checkout_requests WHERE created_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("repository: delete old checkouts: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
