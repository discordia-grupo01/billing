package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
)

var (
	ErrNotFound = model.ErrNotFound
	// ErrInvalidTransition means the purchase is in a state that does not
	// allow the operation (e.g. reversing a purchase that was never paid).
	ErrInvalidTransition = errors.New("repository: invalid status transition")
)

// PointsRepository owns point_purchases, point_wallets and point_ledger.
// Every balance change writes a ledger row in the same transaction, so
// SUM(delta) per user always equals the wallet balance.
type PointsRepository struct {
	pool *pgxpool.Pool
}

func NewPointsRepository(pool *pgxpool.Pool) *PointsRepository {
	return &PointsRepository{pool: pool}
}

const purchaseColumns = `id, user_id, package_id, points, amount_cents, COALESCE(mp_order_id, ''),
	status, COALESCE(status_detail, ''), points_unrecovered, created_at, updated_at`

func scanPurchase(row pgx.Row) (model.PointPurchase, error) {
	var p model.PointPurchase
	err := row.Scan(&p.ID, &p.UserID, &p.PackageID, &p.Points, &p.AmountCents, &p.MPOrderID,
		&p.Status, &p.StatusDetail, &p.PointsUnrecovered, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.PointPurchase{}, ErrNotFound
	}
	return p, err
}

// CreatePurchase stores a new purchase as pending. ID must be set by the
// caller: it is also the external_reference and idempotency key sent to MP.
func (r *PointsRepository) CreatePurchase(ctx context.Context, p model.PointPurchase) (model.PointPurchase, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO point_purchases (id, user_id, package_id, points, amount_cents)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+purchaseColumns,
		p.ID, p.UserID, p.PackageID, p.Points, p.AmountCents)
	created, err := scanPurchase(row)
	if err != nil {
		return model.PointPurchase{}, fmt.Errorf("repository: create purchase: %w", err)
	}
	return created, nil
}

func (r *PointsRepository) SetOrderID(ctx context.Context, purchaseID uuid.UUID, orderID string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE point_purchases SET mp_order_id = $2, updated_at = now() WHERE id = $1`,
		purchaseID, orderID)
	if err != nil {
		return fmt.Errorf("repository: set order id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PointsRepository) GetPurchase(ctx context.Context, id uuid.UUID) (model.PointPurchase, error) {
	return scanPurchase(r.pool.QueryRow(ctx,
		`SELECT `+purchaseColumns+` FROM point_purchases WHERE id = $1`, id))
}

func (r *PointsRepository) GetPurchaseByOrderID(ctx context.Context, orderID string) (model.PointPurchase, error) {
	return scanPurchase(r.pool.QueryRow(ctx,
		`SELECT `+purchaseColumns+` FROM point_purchases WHERE mp_order_id = $1`, orderID))
}

func (r *PointsRepository) ListPurchases(ctx context.Context, userID uuid.UUID, limit int) ([]model.PointPurchase, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+purchaseColumns+` FROM point_purchases WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: list purchases: %w", err)
	}
	defer rows.Close()
	var out []model.PointPurchase
	for rows.Next() {
		p, err := scanPurchase(rows)
		if err != nil {
			return nil, fmt.Errorf("repository: list purchases: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkUnpaid records a failed or expired order. It only moves a purchase
// that is still pending, so a late "failed" notification can never undo a
// purchase that was already paid. Returns false if nothing changed.
func (r *PointsRepository) MarkUnpaid(ctx context.Context, purchaseID uuid.UUID, status model.PurchaseStatus, detail string) (bool, error) {
	if status != model.PurchaseStatusFailed && status != model.PurchaseStatusExpired {
		return false, fmt.Errorf("repository: mark unpaid: unexpected status %q", status)
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE point_purchases SET status = $2, status_detail = NULLIF($3, ''), updated_at = now()
		WHERE id = $1 AND status = 'pending'`,
		purchaseID, status, detail)
	if err != nil {
		return false, fmt.Errorf("repository: mark unpaid: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CreditPurchase marks the purchase paid and adds its points to the wallet,
// in one transaction. MP is the source of truth, so a purchase we had marked
// failed or expired is still credited if MP later reports it accredited.
// Returns false, without error, if the purchase was already credited.
func (r *PointsRepository) CreditPurchase(ctx context.Context, purchaseID uuid.UUID, statusDetail string) (bool, error) {
	credited := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		p, err := scanPurchase(tx.QueryRow(ctx,
			`SELECT `+purchaseColumns+` FROM point_purchases WHERE id = $1 FOR UPDATE`, purchaseID))
		if err != nil {
			return err
		}
		switch p.Status {
		case model.PurchaseStatusPaid, model.PurchaseStatusRefunded, model.PurchaseStatusChargedBack:
			return nil
		}
		if p.MPOrderID == "" {
			return fmt.Errorf("%w: purchase %s has no order id", ErrInvalidTransition, p.ID)
		}

		if err := ensureWallet(ctx, tx, p.UserID); err != nil {
			return err
		}
		inserted, err := insertLedger(ctx, tx, p.UserID, int64(p.Points), model.LedgerReasonPurchase, p.MPOrderID)
		if err != nil || !inserted {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE point_wallets SET balance = balance + $2, updated_at = now() WHERE user_id = $1`,
			p.UserID, p.Points); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE point_purchases SET status = 'paid', status_detail = NULLIF($2, ''), updated_at = now()
			WHERE id = $1`, p.ID, statusDetail); err != nil {
			return err
		}
		credited = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("repository: credit purchase: %w", err)
	}
	return credited, nil
}

// ReversePurchase takes back the points of a refunded or charged-back
// purchase. The balance cannot go below zero, so only what is left is
// deducted and the rest is stored in points_unrecovered. Returns the points
// actually deducted; 0 with no error if it was already reversed.
func (r *PointsRepository) ReversePurchase(ctx context.Context, purchaseID uuid.UUID, reason model.LedgerReason) (int64, error) {
	var newStatus model.PurchaseStatus
	switch reason {
	case model.LedgerReasonRefund:
		newStatus = model.PurchaseStatusRefunded
	case model.LedgerReasonChargeback:
		newStatus = model.PurchaseStatusChargedBack
	default:
		return 0, fmt.Errorf("repository: reverse purchase: unexpected reason %q", reason)
	}

	var deducted int64
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		p, err := scanPurchase(tx.QueryRow(ctx,
			`SELECT `+purchaseColumns+` FROM point_purchases WHERE id = $1 FOR UPDATE`, purchaseID))
		if err != nil {
			return err
		}
		switch p.Status {
		case model.PurchaseStatusRefunded, model.PurchaseStatusChargedBack:
			return nil
		case model.PurchaseStatusPaid:
		default:
			return fmt.Errorf("%w: cannot reverse a %s purchase", ErrInvalidTransition, p.Status)
		}

		var balance int64
		if err := tx.QueryRow(ctx,
			`SELECT balance FROM point_wallets WHERE user_id = $1 FOR UPDATE`, p.UserID).Scan(&balance); err != nil {
			return err
		}
		take := min(balance, int64(p.Points))

		inserted, err := insertLedger(ctx, tx, p.UserID, -take, reason, p.MPOrderID)
		if err != nil || !inserted {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE point_wallets SET balance = balance - $2, updated_at = now() WHERE user_id = $1`,
			p.UserID, take); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE point_purchases SET status = $2, points_unrecovered = $3, updated_at = now()
			WHERE id = $1`, p.ID, newStatus, int64(p.Points)-take); err != nil {
			return err
		}
		deducted = take
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("repository: reverse purchase: %w", err)
	}
	return deducted, nil
}

// Balance returns 0 for users that never had points.
func (r *PointsRepository) Balance(ctx context.Context, userID uuid.UUID) (int64, error) {
	var balance int64
	err := r.pool.QueryRow(ctx, `SELECT balance FROM point_wallets WHERE user_id = $1`, userID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("repository: balance: %w", err)
	}
	return balance, nil
}

func (r *PointsRepository) ListLedger(ctx context.Context, userID uuid.UUID, limit int) ([]model.LedgerEntry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, user_id, delta, reason, ref_id, created_at FROM point_ledger
		WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("repository: list ledger: %w", err)
	}
	defer rows.Close()
	var out []model.LedgerEntry
	for rows.Next() {
		var e model.LedgerEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Delta, &e.Reason, &e.RefID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("repository: list ledger: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func ensureWallet(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO point_wallets (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID)
	return err
}

// insertLedger relies on UNIQUE(reason, ref_id): false means this exact
// movement was already recorded and the caller must not touch the balance.
func insertLedger(ctx context.Context, tx pgx.Tx, userID uuid.UUID, delta int64, reason model.LedgerReason, refID string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO point_ledger (user_id, delta, reason, ref_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (reason, ref_id) DO NOTHING`, userID, delta, reason, refID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
