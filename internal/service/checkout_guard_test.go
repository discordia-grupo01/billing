package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

type fakeCheckoutStore struct {
	rows        map[model.CheckoutKey]*model.CheckoutReservation
	reserveErr  error
	failSave    error
	staleBefore time.Time
	deleted     time.Time
}

func newFakeCheckoutStore() *fakeCheckoutStore {
	return &fakeCheckoutStore{rows: map[model.CheckoutKey]*model.CheckoutReservation{}}
}

func (f *fakeCheckoutStore) Reserve(_ context.Context, k model.CheckoutKey, fingerprint string, staleBeforee time.Time) (model.CheckoutReservation, error) {
	f.staleBefore = staleBeforee
	if f.reserveErr != nil {
		return model.CheckoutReservation{}, f.reserveErr
	}
	if row, ok := f.rows[k]; ok {
		return *row, nil
	}
	f.rows[k] = &model.CheckoutReservation{Fingerprint: fingerprint}
	return model.CheckoutReservation{Acquired: true, Fingerprint: fingerprint}, nil
}

func (f *fakeCheckoutStore) Complete(_ context.Context, k model.CheckoutKey, purchaseID uuid.UUID, checkoutURL string) error {
	if f.failSave != nil {
		return f.failSave
	}
	f.rows[k].PurchaseID = purchaseID
	f.rows[k].CheckoutURL = checkoutURL
	return nil
}

func (f *fakeCheckoutStore) Release(_ context.Context, k model.CheckoutKey) error {
	if f.failSave != nil {
		return f.failSave
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeCheckoutStore) DeleteOlderThan(_ context.Context, before time.Time) (int, error) {
	f.deleted = before
	return 3, nil
}

var guardKey = model.CheckoutKey{UserID: userID, Scope: model.CheckoutScopePoints, Key: "k1"}

func countingCheckout(calls *int, result Checkout) func(context.Context) (Checkout, error) {
	return func(context.Context) (Checkout, error) {
		*calls++
		return result, nil
	}
}

func TestCheckoutGuardReplaysTheFirstResult(t *testing.T) {
	g := NewCheckoutGuard(newFakeCheckoutStore(), quietLogger())
	want := Checkout{PurchaseID: uuid.New(), CheckoutURL: "https://mp.test/1"}
	calls := 0

	for range 2 {
		got, err := g.Run(context.Background(), guardKey, "points_100", countingCheckout(&calls, want))
		if err != nil || got != want {
			t.Fatalf("Run = %+v, %v; want %+v", got, err, want)
		}
	}
	if calls != 1 {
		t.Errorf("checkout ran %d times, want 1", calls)
	}
}

func TestCheckoutGuardWithoutKeyAlwaysRuns(t *testing.T) {
	store := newFakeCheckoutStore()
	g := NewCheckoutGuard(store, quietLogger())
	k := guardKey
	k.Key = ""
	calls := 0

	for range 2 {
		if _, err := g.Run(context.Background(), k, "points_100", countingCheckout(&calls, Checkout{CheckoutURL: "u"})); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || len(store.rows) != 0 {
		t.Errorf("calls = %d, rows = %d; want 2 and none", calls, len(store.rows))
	}
}

func TestCheckoutGuardRejects(t *testing.T) {
	cases := map[string]struct {
		row         model.CheckoutReservation
		fingerprint string
		want        error
	}{
		"in progress": {
			row: model.CheckoutReservation{Fingerprint: "points_100"}, fingerprint: "points_100", want: ErrCheckoutInProgress,
		},
		"key reused for another package": {
			row:         model.CheckoutReservation{Fingerprint: "points_100", CheckoutURL: "u"},
			fingerprint: "points_550", want: ErrCheckoutKeyReused,
		},
		"key reused while in progress": {
			row: model.CheckoutReservation{Fingerprint: "points_100"}, fingerprint: "points_550", want: ErrCheckoutKeyReused,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeCheckoutStore()
			row := tc.row
			store.rows[guardKey] = &row
			calls := 0

			_, err := NewCheckoutGuard(store, quietLogger()).Run(context.Background(), guardKey, tc.fingerprint, countingCheckout(&calls, Checkout{}))
			if !errors.Is(err, tc.want) || calls != 0 {
				t.Errorf("err = %v, calls = %d; want %v and no checkout", err, calls, tc.want)
			}
		})
	}
}

func TestCheckoutGuardReleasesTheKeyOfAFailedCheckout(t *testing.T) {
	store := newFakeCheckoutStore()
	g := NewCheckoutGuard(store, quietLogger())

	_, err := g.Run(context.Background(), guardKey, "points_100", func(context.Context) (Checkout, error) {
		return Checkout{}, errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}

	want := Checkout{CheckoutURL: "https://mp.test/retry"}
	calls := 0
	got, err := g.Run(context.Background(), guardKey, "points_100", countingCheckout(&calls, want))
	if err != nil || got != want || calls != 1 {
		t.Errorf("retry = %+v, %v, calls %d", got, err, calls)
	}
}

func TestCheckoutGuardReserveError(t *testing.T) {
	store := newFakeCheckoutStore()
	store.reserveErr = errBoom
	calls := 0

	_, err := NewCheckoutGuard(store, quietLogger()).Run(context.Background(), guardKey, "x", countingCheckout(&calls, Checkout{}))
	if !errors.Is(err, errBoom) || calls != 0 {
		t.Errorf("err = %v, calls = %d; want errBoom and no checkout", err, calls)
	}
}

func TestCheckoutGuardStillReturnsACheckoutItCouldNotSave(t *testing.T) {
	store := newFakeCheckoutStore()
	store.failSave = errBoom
	want := Checkout{CheckoutURL: "https://mp.test/1"}
	calls := 0

	got, err := NewCheckoutGuard(store, quietLogger()).Run(context.Background(), guardKey, "x", countingCheckout(&calls, want))
	if err != nil || got != want {
		t.Errorf("Run = %+v, %v; want %+v", got, err, want)
	}
}

func TestCheckoutGuardStillReturnsTheErrorIfReleaseFails(t *testing.T) {
	store := newFakeCheckoutStore()
	store.failSave = errors.New("db down")

	_, err := NewCheckoutGuard(store, quietLogger()).Run(context.Background(), guardKey, "x", func(context.Context) (Checkout, error) {
		return Checkout{}, errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want errBoom", err)
	}
}

func TestCheckoutGuardSavesEvenIfTheClientHungUp(t *testing.T) {
	store := newFakeCheckoutStore()
	g := NewCheckoutGuard(store, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())

	_, err := g.Run(ctx, guardKey, "x", func(context.Context) (Checkout, error) {
		cancel()
		return Checkout{CheckoutURL: "u"}, nil
	})
	if err != nil || store.rows[guardKey].CheckoutURL != "u" {
		t.Errorf("err = %v, row = %+v", err, store.rows[guardKey])
	}
}

func TestCheckoutGuardTimes(t *testing.T) {
	store := newFakeCheckoutStore()
	g := NewCheckoutGuard(store, quietLogger())
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }

	calls := 0
	if _, err := g.Run(context.Background(), guardKey, "x", countingCheckout(&calls, Checkout{CheckoutURL: "u"})); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-checkoutLockTTL); !store.staleBefore.Equal(want) {
		t.Errorf("stale before = %v, want %v", store.staleBefore, want)
	}
	if n, err := g.DeleteExpired(context.Background()); err != nil || n != 3 {
		t.Fatalf("DeleteExpired = %d, %v", n, err)
	}
	if want := now.Add(-checkoutKeyTTL); !store.deleted.Equal(want) {
		t.Errorf("deleted before = %v, want %v", store.deleted, want)
	}
}

func TestNewCheckoutGuardDefaultLogger(t *testing.T) {
	if g := NewCheckoutGuard(newFakeCheckoutStore(), nil); g.log == nil {
		t.Error("logger should default to slog.Default")
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
