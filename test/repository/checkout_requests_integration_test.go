package repository_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/repository"
)

func newCheckoutKey() model.CheckoutKey {
	return model.CheckoutKey{UserID: uuid.New(), Scope: model.CheckoutScopePoints, Key: uuid.NewString()}
}

func TestCheckoutRequestLifecycle(t *testing.T) {
	repo := repository.NewCheckoutRequestRepository(newTestPool(t))
	ctx := context.Background()
	k := newCheckoutKey()
	fresh := time.Now().Add(-2 * time.Minute)

	first, err := repo.Reserve(ctx, k, "points_100", fresh)
	if err != nil || !first.Acquired {
		t.Fatalf("first Reserve = %+v, %v; want acquired", first, err)
	}
	inProgress, err := repo.Reserve(ctx, k, "points_100", fresh)
	if err != nil || inProgress.Acquired || inProgress.CheckoutURL != "" {
		t.Fatalf("second Reserve = %+v, %v; want in progress", inProgress, err)
	}

	purchaseID := uuid.New()
	if err := repo.Complete(ctx, k, purchaseID, "https://mp.test/1"); err != nil {
		t.Fatal(err)
	}
	done, err := repo.Reserve(ctx, k, "points_100", time.Now().Add(time.Hour))
	if err != nil || done.Acquired || done.CheckoutURL != "https://mp.test/1" || done.PurchaseID != purchaseID {
		t.Errorf("after Complete = %+v, %v; a finished checkout is never taken over", done, err)
	}
}

func TestCheckoutRequestWithoutPurchase(t *testing.T) {
	repo := repository.NewCheckoutRequestRepository(newTestPool(t))
	ctx := context.Background()
	k := newCheckoutKey()
	k.Scope = model.CheckoutScopePremium

	if _, err := repo.Reserve(ctx, k, "premium", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repo.Complete(ctx, k, uuid.Nil, "https://mp.test/sub"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Reserve(ctx, k, "premium", time.Now().Add(-time.Hour))
	if err != nil || got.PurchaseID != uuid.Nil || got.CheckoutURL != "https://mp.test/sub" {
		t.Errorf("Reserve = %+v, %v", got, err)
	}
}

func TestCheckoutRequestKeysAreSeparated(t *testing.T) {
	repo := repository.NewCheckoutRequestRepository(newTestPool(t))
	ctx := context.Background()
	k := newCheckoutKey()
	if _, err := repo.Reserve(ctx, k, "x", time.Now()); err != nil {
		t.Fatal(err)
	}

	otherUser, otherScope := k, k
	otherUser.UserID = uuid.New()
	otherScope.Scope = model.CheckoutScopePremium
	for name, other := range map[string]model.CheckoutKey{"user": otherUser, "scope": otherScope} {
		if got, err := repo.Reserve(ctx, other, "x", time.Now()); err != nil || !got.Acquired {
			t.Errorf("another %s should get its own key: %+v, %v", name, got, err)
		}
	}
}

func TestCheckoutRequestReleaseAndStaleTakeover(t *testing.T) {
	repo := repository.NewCheckoutRequestRepository(newTestPool(t))
	ctx := context.Background()
	k := newCheckoutKey()
	if _, err := repo.Reserve(ctx, k, "x", time.Now()); err != nil {
		t.Fatal(err)
	}

	if got, err := repo.Reserve(ctx, k, "y", time.Now().Add(time.Hour)); err != nil || !got.Acquired || got.Fingerprint != "y" {
		t.Errorf("a dead request's key should be taken over: %+v, %v", got, err)
	}

	if err := repo.Release(ctx, k); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Reserve(ctx, k, "x", time.Now().Add(-time.Hour)); err != nil || !got.Acquired {
		t.Errorf("a released key should be free: %+v, %v", got, err)
	}

	if err := repo.Complete(ctx, k, uuid.Nil, "u"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Release(ctx, k); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Reserve(ctx, k, "x", time.Now().Add(-time.Hour)); got.CheckoutURL != "u" {
		t.Errorf("a finished checkout should survive Release: %+v", got)
	}
}

func TestCheckoutRequestOnlyOneConcurrentWinner(t *testing.T) {
	repo := repository.NewCheckoutRequestRepository(newTestPool(t))
	ctx := context.Background()
	k := newCheckoutKey()

	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := repo.Reserve(ctx, k, "x", time.Now().Add(-time.Minute))
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			if got.Acquired {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("%d requests won the key, want 1", got)
	}
}

func TestCheckoutRequestDeleteOlderThan(t *testing.T) {
	repo := repository.NewCheckoutRequestRepository(newTestPool(t))
	ctx := context.Background()
	k := newCheckoutKey()
	if _, err := repo.Reserve(ctx, k, "x", time.Now()); err != nil {
		t.Fatal(err)
	}

	if n, err := repo.DeleteOlderThan(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("recent key: deleted %d, %v", n, err)
	}
	if n, err := repo.DeleteOlderThan(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("old key: deleted %d, %v", n, err)
	}
}
