package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

func newSimulator(store *fakePremiumStore) *PremiumSimulator {
	s := NewPremiumSimulator(newPremiumService(store, &fakeGateway{}, PremiumConfig{}), store)
	s.newID = func() uuid.UUID { return purchaseID }
	return s
}

// CA3: an approved renewal extends Premium a month from premium_until, with
// no gap in the benefits.
func TestSimulateApprovedRenewal(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedPaidPremium(store)
	until := *store.premium[userID].PremiumUntil

	view, err := newSimulator(store).SimulateCharge(context.Background(), userID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := until.AddDate(0, 1, 0)
	if view.State != PremiumStateActive || view.PremiumUntil == nil || !view.PremiumUntil.Equal(want) {
		t.Errorf("view = %+v, want active until %v", view, want)
	}
	c := store.charges["sim-"+purchaseID.String()]
	if c.PreapprovalID != "PRE1" || c.AmountCents != premiumPricePesos*100 || c.AppliedAt == nil {
		t.Errorf("simulated charge not applied like a real one: %+v", c)
	}
}

// CA4, first half: a rejected renewal changes nothing yet.
func TestSimulateRejectedRenewal(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedPaidPremium(store)
	until := *store.premium[userID].PremiumUntil

	view, err := newSimulator(store).SimulateCharge(context.Background(), userID, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.PremiumUntil == nil || !view.PremiumUntil.Equal(until) {
		t.Errorf("a rejected charge must not extend Premium: %+v", view)
	}
	if c := store.charges["sim-"+purchaseID.String()]; c.PaymentStatus != "rejected" || c.AppliedAt != nil {
		t.Errorf("rejected charge should be stored but not applied: %+v", c)
	}
}

func TestSimulateChargeNeedsALiveSubscription(t *testing.T) {
	for name, seed := range map[string]func(*fakePremiumStore){
		"never subscribed": func(*fakePremiumStore) {},
		"cancelled":        func(s *fakePremiumStore) { seedSub(s, "PRE1", model.SubscriptionStatusCancelled) },
		"pending":          func(s *fakePremiumStore) { seedSub(s, "PRE1", model.SubscriptionStatusPending) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seed(store)
			if _, err := newSimulator(store).SimulateCharge(context.Background(), userID, true); !errors.Is(err, ErrNoLiveSubscription) {
				t.Errorf("err = %v, want ErrNoLiveSubscription", err)
			}
		})
	}
}

func TestSimulateChargeErrors(t *testing.T) {
	for _, step := range []string{"LatestSubscription", "ApplyCharge"} {
		t.Run(step, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedPaidPremium(store)
			store.failOn = map[string]error{step: errBoom}
			if _, err := newSimulator(store).SimulateCharge(context.Background(), userID, true); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want errBoom", err)
			}
		})
	}
}

// CA4, second half: when the paid period ends, Premium is deactivated and the
// expiry job tells the rest of Discordia.
func TestSimulateExpiry(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedPaidPremium(store)

	view, err := newSimulator(store).SimulateExpiry(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.State != PremiumStateExpired {
		t.Errorf("state = %s, want expired", view.State)
	}
	if len(store.expireLimits) == 0 {
		t.Error("the expiry job should run so the change is published")
	}
}

func TestSimulateExpiryWithoutActivePremium(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedPremium(store, premiumNow.AddDate(0, 0, -1), false)
	if _, err := newSimulator(store).SimulateExpiry(context.Background(), userID); !errors.Is(err, ErrNoActivePremium) {
		t.Errorf("err = %v, want ErrNoActivePremium", err)
	}
}

func TestSimulateExpiryErrors(t *testing.T) {
	for _, step := range []string{"EndPremiumPeriod", "ExpireDue"} {
		t.Run(step, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedPaidPremium(store)
			store.failOn = map[string]error{step: errBoom}
			if _, err := newSimulator(store).SimulateExpiry(context.Background(), userID); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want errBoom", err)
			}
		})
	}
}
