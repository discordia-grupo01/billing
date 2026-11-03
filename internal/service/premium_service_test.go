package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

var premiumNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func newPremiumService(store PremiumStore, gw *fakeGateway, cfg PremiumConfig) *PremiumService {
	s := NewPremiumService(store, gw, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return premiumNow }
	return s
}

func seedSub(store *fakePremiumStore, id, status string) {
	store.subs = append(store.subs, model.SubscriptionRecord{
		MPPreapprovalID: id, UserID: userID, MPStatus: status, AmountCents: premiumPricePesos * 100, CurrencyID: "ARS",
	})
}

func seedPremium(store *fakePremiumStore, until time.Time, renews bool) {
	store.premium[userID] = model.Premium{UserID: userID, PremiumUntil: &until, Renews: renews}
}

// seedAppliedCharge stores a charge of PRE1 that already extended Premium.
func seedAppliedCharge(store *fakePremiumStore, paymentID string) {
	applied := premiumNow.AddDate(0, 0, -10)
	store.charges["AP1"] = model.Charge{
		AuthorizedPaymentID: "AP1", PreapprovalID: "PRE1", PaymentID: paymentID,
		PaymentStatus: model.PaymentStatusApproved, AppliedAt: &applied,
	}
}

func mpSub(id, status string) model.Subscription {
	return model.Subscription{ID: id, Status: status, ExternalReference: userID.String()}
}

func approvedCharge(id string, debit time.Time) model.Charge {
	return model.Charge{
		AuthorizedPaymentID: id, PreapprovalID: "PRE1", PaymentID: "PAY-" + id, Status: "processed",
		PaymentStatus: model.PaymentStatusApproved, AmountCents: premiumPricePesos * 100, DebitDate: &debit,
	}
}

func TestPremiumStartCheckout(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	gw := &fakeGateway{sub: model.Subscription{ID: "PRE1", Status: "pending", InitPoint: "https://mp.test/sub"}}
	s := newPremiumService(store, gw, PremiumConfig{BackURL: "https://discordia.test/return?kind=premium"})

	url, err := s.StartCheckout(context.Background(), userID, "ana@test.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if url != "https://mp.test/sub" {
		t.Errorf("url = %q", url)
	}
	req := gw.subReq
	if req.ExternalReference != userID.String() || req.PayerEmail != "ana@test.com" || req.Reason != premiumReason ||
		req.AmountCents != premiumPricePesos*100 || req.CurrencyID != "ARS" || req.BackURL != "https://discordia.test/return?kind=premium" {
		t.Errorf("unexpected subscription request: %+v", req)
	}
	if len(store.subs) != 1 {
		t.Fatalf("got %d subscriptions, want 1", len(store.subs))
	}
	if r := store.subs[0]; r.MPPreapprovalID != "PRE1" || r.UserID != userID || r.MPStatus != "pending" ||
		r.AmountCents != premiumPricePesos*100 || r.CurrencyID != "ARS" {
		t.Errorf("unexpected record: %+v", r)
	}
}

func TestPremiumStartCheckoutUsesTestPayerEmail(t *testing.T) {
	gw := &fakeGateway{sub: model.Subscription{ID: "PRE1", Status: "pending"}}
	s := newPremiumService(newFakePremiumStore(premiumNow), gw, PremiumConfig{TestPayerEmail: "test_user@testuser.com"})

	if _, err := s.StartCheckout(context.Background(), userID, "ana@test.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gw.subReq.PayerEmail != "test_user@testuser.com" {
		t.Errorf("PayerEmail = %q, want the sandbox buyer", gw.subReq.PayerEmail)
	}
}

func TestPremiumStartCheckoutLatestSubscription(t *testing.T) {
	tests := map[string]error{
		model.SubscriptionStatusAuthorized: ErrAlreadySubscribed,
		model.SubscriptionStatusPaused:     ErrAlreadySubscribed,
		model.SubscriptionStatusPending:    nil,
		model.SubscriptionStatusCancelled:  nil,
	}
	for status, wantErr := range tests {
		t.Run(status, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedSub(store, "PRE0", status)
			gw := &fakeGateway{sub: model.Subscription{ID: "PRE1", Status: "pending"}}
			s := newPremiumService(store, gw, PremiumConfig{})

			_, err := s.StartCheckout(context.Background(), userID, "ana@test.com")
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
			if wantErr != nil && gw.subReq.ExternalReference != "" {
				t.Error("no subscription should be created in MP")
			}
		})
	}
}

func TestPremiumStartCheckoutErrors(t *testing.T) {
	for _, step := range []string{"LatestSubscription", "CreateSubscription"} {
		t.Run("store "+step, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			store.failOn = map[string]error{step: errBoom}
			s := newPremiumService(store, &fakeGateway{sub: mpSub("PRE1", "pending")}, PremiumConfig{})
			if _, err := s.StartCheckout(context.Background(), userID, ""); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want errBoom", err)
			}
		})
	}
	t.Run("gateway", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		s := newPremiumService(store, &fakeGateway{failOn: map[string]error{"CreateSubscription": errBoom}}, PremiumConfig{})
		if _, err := s.StartCheckout(context.Background(), userID, ""); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
		if len(store.subs) != 0 {
			t.Error("nothing should be stored")
		}
	})
}

func TestPremiumCancel(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
	until := premiumNow.AddDate(0, 0, 20)
	seedPremium(store, until, true)
	gw := &fakeGateway{sub: mpSub("PRE1", model.SubscriptionStatusAuthorized)}
	s := newPremiumService(store, gw, PremiumConfig{})

	v, err := s.Cancel(context.Background(), userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !slices.Equal(gw.cancelled, []string{"PRE1"}) {
		t.Errorf("cancelled in MP: %v", gw.cancelled)
	}
	if v.State != PremiumStateActiveCanceled || v.Renews || v.PremiumUntil == nil || !v.PremiumUntil.Equal(until) ||
		v.MPStatus != model.SubscriptionStatusCancelled {
		t.Errorf("Premium must last until the paid date without renewing: %+v", v)
	}
}

func TestPremiumCancelWithoutLiveSubscription(t *testing.T) {
	for name, seed := range map[string]func(*fakePremiumStore){
		"never subscribed": func(*fakePremiumStore) {},
		"already cancelled": func(s *fakePremiumStore) {
			seedSub(s, "PRE1", model.SubscriptionStatusCancelled)
		},
		"never authorized": func(s *fakePremiumStore) { seedSub(s, "PRE1", model.SubscriptionStatusPending) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seed(store)
			gw := &fakeGateway{}
			s := newPremiumService(store, gw, PremiumConfig{})
			if _, err := s.Cancel(context.Background(), userID); !errors.Is(err, ErrNoLiveSubscription) {
				t.Errorf("err = %v, want ErrNoLiveSubscription", err)
			}
			if len(gw.cancelled) != 0 {
				t.Errorf("nothing should be cancelled in MP: %v", gw.cancelled)
			}
		})
	}
}

func TestPremiumCancelErrors(t *testing.T) {
	for _, step := range []string{"LatestSubscription", "SyncSubscriptionStatus"} {
		t.Run("store "+step, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
			store.failOn = map[string]error{step: errBoom}
			s := newPremiumService(store, &fakeGateway{sub: mpSub("PRE1", "authorized")}, PremiumConfig{})
			if _, err := s.Cancel(context.Background(), userID); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want errBoom", err)
			}
		})
	}
	t.Run("gateway", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
		s := newPremiumService(store, &fakeGateway{failOn: map[string]error{"CancelSubscription": errBoom}}, PremiumConfig{})
		if _, err := s.Cancel(context.Background(), userID); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
}

func TestPremiumState(t *testing.T) {
	future := premiumNow.AddDate(0, 0, 10)
	past := premiumNow.AddDate(0, 0, -1)
	tests := []struct {
		name string
		seed func(*fakePremiumStore)
		want PremiumState
	}{
		{"never subscribed", func(*fakePremiumStore) {}, PremiumStateNone},
		{"card not entered yet", func(s *fakePremiumStore) { seedSub(s, "PRE1", "pending") }, PremiumStateNone},
		{"authorized waiting for the first charge", func(s *fakePremiumStore) {
			seedSub(s, "PRE1", "authorized")
		}, PremiumStateProcessingFirstPayment},
		{"active and renewing", func(s *fakePremiumStore) {
			seedSub(s, "PRE1", "authorized")
			seedPremium(s, future, true)
		}, PremiumStateActive},
		{"active but cancelled", func(s *fakePremiumStore) {
			seedSub(s, "PRE1", "cancelled")
			seedPremium(s, future, false)
		}, PremiumStateActiveCanceled},
		{"expired after cancelling", func(s *fakePremiumStore) {
			seedSub(s, "PRE1", "cancelled")
			seedPremium(s, past, false)
		}, PremiumStateExpired},
		{"expired while the renewal is retried", func(s *fakePremiumStore) {
			seedSub(s, "PRE1", "authorized")
			seedAppliedCharge(s, "555")
			seedPremium(s, past, true)
		}, PremiumStateExpired},
		{"resubscribed after expiring", func(s *fakePremiumStore) {
			seedSub(s, "PRE0", "cancelled")
			seedSub(s, "PRE1", "authorized")
			seedPremium(s, past, true)
		}, PremiumStateProcessingFirstPayment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			tt.seed(store)
			s := newPremiumService(store, &fakeGateway{}, PremiumConfig{})

			v, err := s.State(context.Background(), userID, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.State != tt.want {
				t.Errorf("state = %s, want %s", v.State, tt.want)
			}
		})
	}
}

func TestPremiumStateRefreshActivatesWithoutWebhook(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusPending)
	debit := premiumNow.Add(-time.Hour)
	gw := &fakeGateway{
		sub:     mpSub("PRE1", model.SubscriptionStatusAuthorized),
		charges: []model.Charge{approvedCharge("AP1", debit)},
	}
	s := newPremiumService(store, gw, PremiumConfig{})

	v, err := s.State(context.Background(), userID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := debit.AddDate(0, 1, 0)
	if v.State != PremiumStateActive || !v.Renews || v.MPStatus != "authorized" || v.PremiumUntil == nil || !v.PremiumUntil.Equal(want) {
		t.Errorf("unexpected view: %+v (want until %v)", v, want)
	}
}

func TestPremiumStateRefreshToleratesMPFailure(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusPending)
	s := newPremiumService(store, &fakeGateway{failOn: map[string]error{"GetSubscription": errBoom}}, PremiumConfig{})

	v, err := s.State(context.Background(), userID, true)
	if err != nil {
		t.Fatalf("refresh errors must not reach the client: %v", err)
	}
	if v.State != PremiumStateNone {
		t.Errorf("state = %s, want none", v.State)
	}
}

func TestPremiumStateRefreshSkipsFinishedSubscriptions(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusCancelled)
	gw := &fakeGateway{sub: mpSub("PRE1", model.SubscriptionStatusAuthorized)}
	s := newPremiumService(store, gw, PremiumConfig{})

	if _, err := s.State(context.Background(), userID, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.subs[0].MPStatus != model.SubscriptionStatusCancelled {
		t.Error("a cancelled subscription should not be re-synced on refresh")
	}
}

func TestPremiumStateErrors(t *testing.T) {
	tests := []struct {
		step    string
		refresh bool
	}{{"LatestSubscription", true}, {"LatestSubscription", false}, {"GetPremium", false}, {"HasAppliedCharge", false}}
	for _, tt := range tests {
		t.Run(tt.step, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
			store.failOn = map[string]error{tt.step: errBoom}
			s := newPremiumService(store, &fakeGateway{}, PremiumConfig{})
			if _, err := s.State(context.Background(), userID, tt.refresh); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want errBoom", err)
			}
		})
	}
}

func TestProcessSubscriptionNotification(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusPending)
	s := newPremiumService(store, &fakeGateway{sub: mpSub("PRE1", model.SubscriptionStatusAuthorized)}, PremiumConfig{})

	if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.subs[0].MPStatus != model.SubscriptionStatusAuthorized || !store.premium[userID].Renews {
		t.Errorf("subscription should be authorized and renewing: %+v %+v", store.subs[0], store.premium[userID])
	}
}

func TestProcessSubscriptionNotificationIgnored(t *testing.T) {
	t.Run("not in our db", func(t *testing.T) {
		s := newPremiumService(newFakePremiumStore(premiumNow), &fakeGateway{sub: mpSub("PRE1", "authorized")}, PremiumConfig{})
		if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("unknown to MP", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		seedSub(store, "PRE1", model.SubscriptionStatusPending)
		s := newPremiumService(store, &fakeGateway{}, PremiumConfig{})
		if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestProcessSubscriptionNotificationMismatch(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusPending)
	other := mpSub("PRE1", model.SubscriptionStatusAuthorized)
	other.ExternalReference = uuid.NewString()
	s := newPremiumService(store, &fakeGateway{sub: other}, PremiumConfig{})

	if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); !errors.Is(err, ErrSubscriptionMismatch) {
		t.Fatalf("err = %v, want ErrSubscriptionMismatch", err)
	}
	if store.subs[0].MPStatus != model.SubscriptionStatusPending {
		t.Error("status should not change")
	}
}

func TestProcessSubscriptionNotificationCancelsDuplicate(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE0", model.SubscriptionStatusAuthorized)
	seedSub(store, "PRE1", model.SubscriptionStatusPending)
	gw := &fakeGateway{sub: mpSub("PRE1", model.SubscriptionStatusAuthorized)}
	s := newPremiumService(store, gw, PremiumConfig{})

	if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !slices.Equal(gw.cancelled, []string{"PRE1"}) {
		t.Errorf("the duplicate should be cancelled in MP, got %v", gw.cancelled)
	}
	if store.subs[0].MPStatus != model.SubscriptionStatusAuthorized || store.subs[1].MPStatus != model.SubscriptionStatusCancelled {
		t.Errorf("unexpected statuses: %s, %s", store.subs[0].MPStatus, store.subs[1].MPStatus)
	}
}

func TestProcessSubscriptionNotificationErrors(t *testing.T) {
	t.Run("gateway", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		seedSub(store, "PRE1", model.SubscriptionStatusPending)
		s := newPremiumService(store, &fakeGateway{failOn: map[string]error{"GetSubscription": errBoom}}, PremiumConfig{})
		if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	t.Run("cancelling duplicate", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		seedSub(store, "PRE0", model.SubscriptionStatusAuthorized)
		seedSub(store, "PRE1", model.SubscriptionStatusPending)
		gw := &fakeGateway{sub: mpSub("PRE1", "authorized"), failOn: map[string]error{"CancelSubscription": errBoom}}
		s := newPremiumService(store, gw, PremiumConfig{})
		if err := s.ProcessSubscriptionNotification(context.Background(), "PRE1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
}

func TestProcessChargeNotification(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
	debit := premiumNow.Add(-2 * time.Hour)
	s := newPremiumService(store, &fakeGateway{charges: []model.Charge{approvedCharge("AP1", debit)}}, PremiumConfig{})

	for i := 0; i < 2; i++ {
		if err := s.ProcessChargeNotification(context.Background(), "AP1"); err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", i+1, err)
		}
	}

	want := debit.AddDate(0, 1, 0)
	if p := store.premium[userID]; p.PremiumUntil == nil || !p.PremiumUntil.Equal(want) {
		t.Errorf("premium_until = %v, want %v (applied once)", p.PremiumUntil, want)
	}
}

func TestProcessChargeNotificationRejected(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
	charge := approvedCharge("AP1", premiumNow)
	charge.Status, charge.PaymentStatus = "recycling", "rejected"
	s := newPremiumService(store, &fakeGateway{charges: []model.Charge{charge}}, PremiumConfig{})

	if err := s.ProcessChargeNotification(context.Background(), "AP1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.premium[userID].PremiumUntil != nil {
		t.Error("a rejected charge must not give Premium")
	}
	if c := store.charges["AP1"]; c.Status != "recycling" {
		t.Errorf("the charge should still be recorded: %+v", c)
	}
}

func TestProcessChargeNotificationIgnored(t *testing.T) {
	t.Run("unknown to MP", func(t *testing.T) {
		s := newPremiumService(newFakePremiumStore(premiumNow), &fakeGateway{}, PremiumConfig{})
		if err := s.ProcessChargeNotification(context.Background(), "AP1"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("subscription not in our db", func(t *testing.T) {
		gw := &fakeGateway{charges: []model.Charge{approvedCharge("AP1", premiumNow)}}
		s := newPremiumService(newFakePremiumStore(premiumNow), gw, PremiumConfig{})
		if err := s.ProcessChargeNotification(context.Background(), "AP1"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestProcessChargeNotificationErrors(t *testing.T) {
	t.Run("gateway", func(t *testing.T) {
		s := newPremiumService(newFakePremiumStore(premiumNow),
			&fakeGateway{failOn: map[string]error{"GetSubscriptionCharge": errBoom}}, PremiumConfig{})
		if err := s.ProcessChargeNotification(context.Background(), "AP1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	t.Run("store", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		store.failOn = map[string]error{"ApplyCharge": errBoom}
		s := newPremiumService(store, &fakeGateway{charges: []model.Charge{approvedCharge("AP1", premiumNow)}}, PremiumConfig{})
		if err := s.ProcessChargeNotification(context.Background(), "AP1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
}

// seedPaidPremium leaves a user with an authorized PRE1 whose charge (payment
// 555) gave them Premium until 20 days from now.
func seedPaidPremium(store *fakePremiumStore) {
	seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
	seedAppliedCharge(store, "555")
	seedPremium(store, premiumNow.AddDate(0, 0, 20), true)
}

func TestProcessPaymentNotificationRevokes(t *testing.T) {
	for _, status := range []string{model.PaymentStatusRefunded, model.PaymentStatusChargedBack} {
		t.Run(status, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedPaidPremium(store)
			gw := &fakeGateway{sub: mpSub("PRE1", "authorized"), payment: model.Payment{ID: "555", Status: status}}
			s := newPremiumService(store, gw, PremiumConfig{})

			if err := s.ProcessPaymentNotification(context.Background(), "555"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			p := store.premium[userID]
			if p.PremiumUntil == nil || !p.PremiumUntil.Equal(premiumNow) || p.Renews {
				t.Errorf("Premium should end now without renewal: %+v", p)
			}
			if !slices.Equal(gw.cancelled, []string{"PRE1"}) || store.subs[0].MPStatus != model.SubscriptionStatusCancelled {
				t.Errorf("subscription should be cancelled: MP %v, ours %s", gw.cancelled, store.subs[0].MPStatus)
			}
		})
	}
}

func TestProcessPaymentNotificationRetriesCancel(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedPaidPremium(store)
	gw := &fakeGateway{
		payment: model.Payment{ID: "555", Status: model.PaymentStatusRefunded},
		failOn:  map[string]error{"CancelSubscription": errBoom},
	}
	s := newPremiumService(store, gw, PremiumConfig{})

	if err := s.ProcessPaymentNotification(context.Background(), "555"); !errors.Is(err, errBoom) {
		t.Fatalf("first attempt: err = %v, want errBoom", err)
	}
	gw.failOn = nil
	if err := s.ProcessPaymentNotification(context.Background(), "555"); err != nil {
		t.Fatalf("retry: unexpected error: %v", err)
	}
	if store.subs[0].MPStatus != model.SubscriptionStatusCancelled {
		t.Error("the retry should cancel the subscription even though the charge was already revoked")
	}
}

func TestProcessPaymentNotificationAlreadyCancelled(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedPaidPremium(store)
	store.subs[0].MPStatus = model.SubscriptionStatusCancelled
	gw := &fakeGateway{payment: model.Payment{ID: "555", Status: model.PaymentStatusRefunded}}
	s := newPremiumService(store, gw, PremiumConfig{})

	if err := s.ProcessPaymentNotification(context.Background(), "555"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p := store.premium[userID]; p.PremiumUntil == nil || !p.PremiumUntil.Equal(premiumNow) {
		t.Errorf("Premium should still end now: %+v", p)
	}
	if len(gw.cancelled) != 0 {
		t.Errorf("an already cancelled subscription should not be cancelled again: %v", gw.cancelled)
	}
}

func TestProcessPaymentNotificationIgnored(t *testing.T) {
	tests := map[string]model.Payment{
		"approved payment":     {ID: "555", Status: model.PaymentStatusApproved},
		"not a monthly charge": {ID: "999", Status: model.PaymentStatusRefunded},
	}
	for name, payment := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedPaidPremium(store)
			gw := &fakeGateway{payment: payment}
			s := newPremiumService(store, gw, PremiumConfig{})

			if err := s.ProcessPaymentNotification(context.Background(), payment.ID); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !store.premium[userID].Renews || len(gw.cancelled) != 0 {
				t.Error("nothing should change")
			}
		})
	}
	t.Run("unknown to MP", func(t *testing.T) {
		s := newPremiumService(newFakePremiumStore(premiumNow), &fakeGateway{}, PremiumConfig{})
		if err := s.ProcessPaymentNotification(context.Background(), "555"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestProcessPaymentNotificationErrors(t *testing.T) {
	refunded := model.Payment{ID: "555", Status: model.PaymentStatusRefunded}
	t.Run("gateway", func(t *testing.T) {
		s := newPremiumService(newFakePremiumStore(premiumNow), &fakeGateway{failOn: map[string]error{"GetPayment": errBoom}}, PremiumConfig{})
		if err := s.ProcessPaymentNotification(context.Background(), "555"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	for _, step := range []string{"RevokeCharge", "GetSubscription"} {
		t.Run("store "+step, func(t *testing.T) {
			store := newFakePremiumStore(premiumNow)
			seedPaidPremium(store)
			store.failOn = map[string]error{step: errBoom}
			s := newPremiumService(store, &fakeGateway{payment: refunded}, PremiumConfig{})
			if err := s.ProcessPaymentNotification(context.Background(), "555"); !errors.Is(err, errBoom) {
				t.Errorf("err = %v, want errBoom", err)
			}
		})
	}
}

func TestReconcileSubscription(t *testing.T) {
	store := newFakePremiumStore(premiumNow)
	seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
	first := premiumNow.AddDate(0, -1, 0)
	second := premiumNow
	gw := &fakeGateway{
		sub:     mpSub("PRE1", model.SubscriptionStatusAuthorized),
		charges: []model.Charge{approvedCharge("AP1", first), approvedCharge("AP2", second)},
	}
	s := newPremiumService(store, gw, PremiumConfig{})

	if err := s.ReconcileSubscription(context.Background(), "PRE1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := second.AddDate(0, 1, 0)
	if p := store.premium[userID]; p.PremiumUntil == nil || !p.PremiumUntil.Equal(want) {
		t.Errorf("premium_until = %v, want %v", p.PremiumUntil, want)
	}
}

func TestReconcileSubscriptionErrors(t *testing.T) {
	t.Run("store GetSubscription", func(t *testing.T) {
		s := newPremiumService(newFakePremiumStore(premiumNow), &fakeGateway{}, PremiumConfig{})
		if err := s.ReconcileSubscription(context.Background(), "PRE1"); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("gateway ListSubscriptionCharges", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
		gw := &fakeGateway{sub: mpSub("PRE1", "authorized"), failOn: map[string]error{"ListSubscriptionCharges": errBoom}}
		s := newPremiumService(store, gw, PremiumConfig{})
		if err := s.ReconcileSubscription(context.Background(), "PRE1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	t.Run("store ApplyCharge", func(t *testing.T) {
		store := newFakePremiumStore(premiumNow)
		seedSub(store, "PRE1", model.SubscriptionStatusAuthorized)
		store.failOn = map[string]error{"ApplyCharge": errBoom}
		gw := &fakeGateway{sub: mpSub("PRE1", "authorized"), charges: []model.Charge{approvedCharge("AP1", premiumNow)}}
		s := newPremiumService(store, gw, PremiumConfig{})
		if err := s.ReconcileSubscription(context.Background(), "PRE1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
}

func TestNewPremiumServiceDefaultLogger(t *testing.T) {
	if s := NewPremiumService(newFakePremiumStore(premiumNow), &fakeGateway{}, PremiumConfig{}, nil); s.log == nil {
		t.Error("logger should default to slog.Default")
	}
}
