package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

const (
	// Longer than a checkout can take, so only a dead process loses its key.
	checkoutLockTTL = 2 * time.Minute
	checkoutKeyTTL  = 24 * time.Hour
)

var (
	ErrCheckoutInProgress = errors.New("a checkout with this key is in progress")
	ErrCheckoutKeyReused  = errors.New("idempotency key used with different parameters")
)

type CheckoutRequestStore interface {
	Reserve(ctx context.Context, k model.CheckoutKey, fingerprint string, staleBefore time.Time) (model.CheckoutReservation, error)
	Complete(ctx context.Context, k model.CheckoutKey, purchaseID uuid.UUID, checkoutURL string) error
	Release(ctx context.Context, k model.CheckoutKey) error
	DeleteOlderThan(ctx context.Context, before time.Time) (int, error)
}

// CheckoutGuard makes a checkout idempotent per Idempotency-Key: a double click
// or a client retry gets the first checkout back instead of creating another.
type CheckoutGuard struct {
	store CheckoutRequestStore
	log   *slog.Logger
	now   func() time.Time
}

func NewCheckoutGuard(store CheckoutRequestStore, logger *slog.Logger) *CheckoutGuard {
	if logger == nil {
		logger = slog.Default()
	}
	return &CheckoutGuard{store: store, log: logger, now: time.Now}
}

// Run calls checkout at most once per key. Without a key it just calls it. A
// failed checkout releases the key, so failures aren't replayed. The
// fingerprint detects the same key sent for something else.
func (g *CheckoutGuard) Run(ctx context.Context, k model.CheckoutKey, fingerprint string, checkout func(context.Context) (Checkout, error)) (Checkout, error) {
	if k.Key == "" {
		return checkout(ctx)
	}

	res, err := g.store.Reserve(ctx, k, fingerprint, g.now().Add(-checkoutLockTTL))
	if err != nil {
		return Checkout{}, err
	}
	if !res.Acquired {
		switch {
		case res.Fingerprint != fingerprint:
			return Checkout{}, ErrCheckoutKeyReused
		case res.CheckoutURL == "":
			return Checkout{}, ErrCheckoutInProgress
		}
		return Checkout{PurchaseID: res.PurchaseID, CheckoutURL: res.CheckoutURL}, nil
	}

	// Without the request's context: the client hanging up must not leave the
	// key locked or lose a checkout that was already created.
	store := context.WithoutCancel(ctx)
	result, err := checkout(ctx)
	if err != nil {
		if releaseErr := g.store.Release(store, k); releaseErr != nil {
			g.log.Error("could not release checkout key", "error", releaseErr)
		}
		return Checkout{}, err
	}
	if err := g.store.Complete(store, k, result.PurchaseID, result.CheckoutURL); err != nil {
		g.log.Error("could not save checkout result", "error", err)
	}
	return result, nil
}

// DeleteExpired forgets old keys. A checkout link isn't useful after a day.
func (g *CheckoutGuard) DeleteExpired(ctx context.Context) (int, error) {
	return g.store.DeleteOlderThan(ctx, g.now().Add(-checkoutKeyTTL))
}
