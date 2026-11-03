package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

var (
	userID     = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	purchaseID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func newPointsService(store PointsStore, gw *fakeGateway) *PointsService {
	s := NewPointsService(store, gw, ReturnURLs{
		Success: "https://discordia.test/return?r=success",
		Failure: "https://discordia.test/return?r=failure",
		Pending: "https://discordia.test/return?r=pending",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.newID = func() uuid.UUID { return purchaseID }
	return s
}

// seedPurchase stores a 550 points purchase linked to order ORD1.
func seedPurchase(store *fakePointsStore, status model.PurchaseStatus) {
	store.purchases[purchaseID] = model.PointPurchase{
		ID: purchaseID, UserID: userID, PackageID: "points_550", Points: 550,
		AmountCents: 5_000_00, MPOrderID: "ORD1", Status: status,
	}
	if status == model.PurchaseStatusPaid {
		store.balances[userID] = 550
	}
}

func orderWith(status, detail string) model.PointsOrder {
	return model.PointsOrder{
		ID: "ORD1", Status: status, StatusDetail: detail,
		ExternalReference: purchaseID.String(), AmountCents: 5_000_00,
	}
}

func TestPackages(t *testing.T) {
	s := newPointsService(newFakePointsStore(), &fakeGateway{})

	pkgs := s.Packages()
	want := map[string][2]int64{"points_100": {100, 1_000_00}, "points_550": {550, 5_000_00}, "points_1200": {1200, 10_000_00}}
	if len(pkgs) != len(want) {
		t.Fatalf("got %d packages, want %d", len(pkgs), len(want))
	}
	for _, p := range pkgs {
		if w, ok := want[p.ID]; !ok || int64(p.Points) != w[0] || p.AmountCents != w[1] {
			t.Errorf("unexpected package %+v", p)
		}
	}

	pkgs[0].AmountCents = 1
	if s.Packages()[0].AmountCents == 1 {
		t.Error("Packages must return a copy")
	}
}

func TestStartCheckout(t *testing.T) {
	store := newFakePointsStore()
	gw := &fakeGateway{order: model.PointsOrder{ID: "ORD1", Status: "created", CheckoutURL: "https://mp.test/checkout"}}
	s := newPointsService(store, gw)

	got, err := s.StartCheckout(context.Background(), userID, "points_550",
		CheckoutPayer{Email: "ana@test.com", FirstName: "Ana", LastName: "Pérez"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.PurchaseID != purchaseID || got.CheckoutURL != "https://mp.test/checkout" {
		t.Errorf("unexpected checkout: %+v", got)
	}
	p := store.purchases[purchaseID]
	if p.UserID != userID || p.Points != 550 || p.AmountCents != 5_000_00 || p.MPOrderID != "ORD1" ||
		p.Status != model.PurchaseStatusPending {
		t.Errorf("unexpected purchase: %+v", p)
	}
	req := gw.orderReq
	if req.IdempotencyKey != purchaseID.String() || req.ExternalReference != purchaseID.String() {
		t.Errorf("purchase id must be the idempotency key and external reference: %+v", req)
	}
	if req.ItemID != "points_550" || req.AmountCents != 5_000_00 || req.Title != "550 puntos" ||
		req.PayerEmail != "ana@test.com" || req.PayerFirstName != "Ana" || req.PayerLastName != "Pérez" {
		t.Errorf("unexpected order request: %+v", req)
	}
	if req.SuccessURL == "" || req.FailureURL == "" || req.PendingURL == "" {
		t.Errorf("return URLs not sent: %+v", req)
	}
}

func TestStartCheckoutUnknownPackage(t *testing.T) {
	store := newFakePointsStore()
	s := newPointsService(store, &fakeGateway{})

	_, err := s.StartCheckout(context.Background(), userID, "points_999", CheckoutPayer{})
	if !errors.Is(err, ErrUnknownPackage) {
		t.Fatalf("err = %v, want ErrUnknownPackage", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("nothing should be stored, got calls %v", store.calls)
	}
}

func TestStartCheckoutGatewayFailureLeavesPendingPurchase(t *testing.T) {
	store := newFakePointsStore()
	s := newPointsService(store, &fakeGateway{createErr: errBoom})

	if _, err := s.StartCheckout(context.Background(), userID, "points_100", CheckoutPayer{}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	p := store.purchases[purchaseID]
	if p.Status != model.PurchaseStatusPending || p.MPOrderID != "" {
		t.Errorf("purchase should stay pending without order: %+v", p)
	}
}

func TestStartCheckoutStoreFailures(t *testing.T) {
	store := newFakePointsStore()
	store.err = errBoom
	gw := &fakeGateway{}
	s := newPointsService(store, gw)

	if _, err := s.StartCheckout(context.Background(), userID, "points_100", CheckoutPayer{}); !errors.Is(err, errBoom) {
		t.Fatalf("CreatePurchase failure: err = %v", err)
	}
	if gw.orderReq.IdempotencyKey != "" {
		t.Error("no order should be created if the purchase wasn't stored")
	}

	// SetOrderID fails after the order was created.
	store = newFakePointsStore()
	s = newPointsService(&failOnSetOrderID{store}, &fakeGateway{order: model.PointsOrder{ID: "ORD1"}})
	if _, err := s.StartCheckout(context.Background(), userID, "points_100", CheckoutPayer{}); !errors.Is(err, errBoom) {
		t.Fatalf("SetOrderID failure: err = %v", err)
	}
}

type failOnSetOrderID struct{ *fakePointsStore }

func (f *failOnSetOrderID) SetOrderID(context.Context, uuid.UUID, string) error { return errBoom }

func TestProcessOrderNotification(t *testing.T) {
	tests := []struct {
		name        string
		start       model.PurchaseStatus
		order       model.PointsOrder
		wantStatus  model.PurchaseStatus
		wantBalance int64
		wantDetail  string
	}{
		{"accredited credits the points", model.PurchaseStatusPending,
			orderWith("processed", "accredited"), model.PurchaseStatusPaid, 550, "accredited"},
		{"repeated accredited does not credit twice", model.PurchaseStatusPaid,
			orderWith("processed", "accredited"), model.PurchaseStatusPaid, 550, ""},
		{"late accredited credits a failed purchase", model.PurchaseStatusFailed,
			orderWith("processed", "accredited"), model.PurchaseStatusPaid, 550, "accredited"},
		{"partial refund keeps the points", model.PurchaseStatusPaid,
			orderWith("processed", "partially_refunded"), model.PurchaseStatusPaid, 550, ""},
		{"partial refund of a missed payment credits it", model.PurchaseStatusPending,
			orderWith("processed", "partially_refunded"), model.PurchaseStatusPaid, 550, "partially_refunded"},
		{"failed marks it failed", model.PurchaseStatusPending,
			orderWith("failed", "rejected_by_issuer"), model.PurchaseStatusFailed, 0, "rejected_by_issuer"},
		{"failed by expiry marks it expired", model.PurchaseStatusPending,
			orderWith("failed", "expired"), model.PurchaseStatusExpired, 0, "expired"},
		{"canceled marks it expired", model.PurchaseStatusPending,
			orderWith("canceled", "expired"), model.PurchaseStatusExpired, 0, "expired"},
		{"late failed never undoes a payment", model.PurchaseStatusPaid,
			orderWith("failed", "rejected_by_issuer"), model.PurchaseStatusPaid, 550, ""},
		{"refunded takes the points back", model.PurchaseStatusPaid,
			orderWith("refunded", "refunded"), model.PurchaseStatusRefunded, 0, ""},
		{"charged back in process takes the points back", model.PurchaseStatusPaid,
			orderWith("charged_back", "in_process"), model.PurchaseStatusChargedBack, 0, ""},
		{"charged back reimbursed is not re-credited", model.PurchaseStatusChargedBack,
			orderWith("charged_back", "reimbursed"), model.PurchaseStatusChargedBack, 0, ""},
		{"processing does nothing", model.PurchaseStatusPending,
			orderWith("processing", "in_process"), model.PurchaseStatusPending, 0, ""},
		{"action required does nothing", model.PurchaseStatusPending,
			orderWith("action_required", "waiting_payment"), model.PurchaseStatusPending, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakePointsStore()
			seedPurchase(store, tt.start)
			s := newPointsService(store, &fakeGateway{order: tt.order})

			if err := s.ProcessOrderNotification(context.Background(), "ORD1"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			p := store.purchases[purchaseID]
			if p.Status != tt.wantStatus || p.StatusDetail != tt.wantDetail {
				t.Errorf("purchase = %s/%q, want %s/%q", p.Status, p.StatusDetail, tt.wantStatus, tt.wantDetail)
			}
			if b := store.balances[userID]; b != tt.wantBalance {
				t.Errorf("balance = %d, want %d", b, tt.wantBalance)
			}
		})
	}
}

func TestProcessOrderNotificationRefundOfMissedPayment(t *testing.T) {
	store := newFakePointsStore()
	seedPurchase(store, model.PurchaseStatusPending)
	s := newPointsService(store, &fakeGateway{order: orderWith("refunded", "refunded")})

	if err := s.ProcessOrderNotification(context.Background(), "ORD1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p := store.purchases[purchaseID]; p.Status != model.PurchaseStatusRefunded {
		t.Errorf("status = %s, want refunded", p.Status)
	}
	if len(store.ledger) != 2 || store.ledger[0].Delta != 550 || store.ledger[1].Delta != -550 {
		t.Errorf("ledger should show the credit and its reversal: %+v", store.ledger)
	}
	if store.balances[userID] != 0 {
		t.Errorf("balance = %d, want 0", store.balances[userID])
	}
}

func TestProcessOrderNotificationLinksMissingOrderID(t *testing.T) {
	store := newFakePointsStore()
	seedPurchase(store, model.PurchaseStatusPending)
	p := store.purchases[purchaseID]
	p.MPOrderID = ""
	store.purchases[purchaseID] = p
	s := newPointsService(store, &fakeGateway{order: orderWith("processed", "accredited")})

	if err := s.ProcessOrderNotification(context.Background(), "ORD1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p = store.purchases[purchaseID]
	if p.MPOrderID != "ORD1" || p.Status != model.PurchaseStatusPaid {
		t.Errorf("purchase should be linked and paid: %+v", p)
	}
}

func TestProcessOrderNotificationMismatch(t *testing.T) {
	otherOrder := orderWith("processed", "accredited")
	otherOrder.ID = "ORD2"
	wrongAmount := orderWith("processed", "accredited")
	wrongAmount.AmountCents = 1

	for name, order := range map[string]model.PointsOrder{"order id": otherOrder, "amount": wrongAmount} {
		t.Run(name, func(t *testing.T) {
			store := newFakePointsStore()
			seedPurchase(store, model.PurchaseStatusPending)
			s := newPointsService(store, &fakeGateway{order: order})

			err := s.ProcessOrderNotification(context.Background(), order.ID)
			if !errors.Is(err, ErrOrderMismatch) {
				t.Fatalf("err = %v, want ErrOrderMismatch", err)
			}
			if p := store.purchases[purchaseID]; p.Status != model.PurchaseStatusPending || store.balances[userID] != 0 {
				t.Errorf("nothing should change: %+v", p)
			}
		})
	}
}

func TestProcessOrderNotificationIgnored(t *testing.T) {
	notOurs := orderWith("processed", "accredited")
	notOurs.ExternalReference = "something-else"
	unknownPurchase := orderWith("processed", "accredited")
	unknownPurchase.ExternalReference = uuid.NewString()

	tests := map[string]struct {
		orderID string
		order   model.PointsOrder
	}{
		"order unknown to MP":         {"FAKE", orderWith("processed", "accredited")},
		"external reference not ours": {"ORD1", notOurs},
		"purchase not in our db":      {"ORD1", unknownPurchase},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := newFakePointsStore()
			seedPurchase(store, model.PurchaseStatusPending)
			s := newPointsService(store, &fakeGateway{order: tt.order})

			if err := s.ProcessOrderNotification(context.Background(), tt.orderID); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if slices.ContainsFunc(store.calls, func(c string) bool { return c != "GetPurchase" }) {
				t.Errorf("nothing should be written, got calls %v", store.calls)
			}
		})
	}
}

func TestProcessOrderNotificationErrors(t *testing.T) {
	t.Run("gateway", func(t *testing.T) {
		s := newPointsService(newFakePointsStore(), &fakeGateway{getErr: errBoom})
		if err := s.ProcessOrderNotification(context.Background(), "ORD1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	t.Run("store", func(t *testing.T) {
		store := newFakePointsStore()
		seedPurchase(store, model.PurchaseStatusPending)
		store.err = errBoom
		s := newPointsService(store, &fakeGateway{order: orderWith("processed", "accredited")})
		if err := s.ProcessOrderNotification(context.Background(), "ORD1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	t.Run("link order", func(t *testing.T) {
		store := newFakePointsStore()
		seedPurchase(store, model.PurchaseStatusPending)
		p := store.purchases[purchaseID]
		p.MPOrderID = ""
		store.purchases[purchaseID] = p
		s := newPointsService(&failOnSetOrderID{store}, &fakeGateway{order: orderWith("processed", "accredited")})
		if err := s.ProcessOrderNotification(context.Background(), "ORD1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
	t.Run("credit before reversal", func(t *testing.T) {
		store := newFakePointsStore()
		seedPurchase(store, model.PurchaseStatusPending)
		s := newPointsService(&failOnCredit{store}, &fakeGateway{order: orderWith("refunded", "refunded")})
		if err := s.ProcessOrderNotification(context.Background(), "ORD1"); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want errBoom", err)
		}
	})
}

type failOnCredit struct{ *fakePointsStore }

func (f *failOnCredit) CreditPurchase(context.Context, uuid.UUID, string) (bool, error) {
	return false, errBoom
}

func TestGetPurchaseOnlyForOwner(t *testing.T) {
	store := newFakePointsStore()
	seedPurchase(store, model.PurchaseStatusPaid)
	s := newPointsService(store, &fakeGateway{})
	ctx := context.Background()

	if p, err := s.GetPurchase(ctx, userID, purchaseID); err != nil || p.ID != purchaseID {
		t.Errorf("owner: got %+v, %v", p, err)
	}
	if _, err := s.GetPurchase(ctx, uuid.New(), purchaseID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("other user: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetPurchase(ctx, userID, uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
}

func TestQueries(t *testing.T) {
	store := newFakePointsStore()
	seedPurchase(store, model.PurchaseStatusPaid)
	s := newPointsService(store, &fakeGateway{})
	ctx := context.Background()

	if b, err := s.Balance(ctx, userID); err != nil || b != 550 {
		t.Errorf("Balance = %d, %v", b, err)
	}
	for limit, want := range map[int]int{0: defaultListLimit, -5: defaultListLimit, 10: 10, 1000: maxListLimit} {
		if _, err := s.ListPurchases(ctx, userID, limit); err != nil || store.lastLimit != want {
			t.Errorf("ListPurchases(%d) used limit %d, want %d (err %v)", limit, store.lastLimit, want, err)
		}
		if _, err := s.ListLedger(ctx, userID, limit); err != nil || store.lastLimit != want {
			t.Errorf("ListLedger(%d) used limit %d, want %d (err %v)", limit, store.lastLimit, want, err)
		}
	}
}

func TestNewPointsServiceDefaultLogger(t *testing.T) {
	if s := NewPointsService(newFakePointsStore(), &fakeGateway{}, ReturnURLs{}, nil); s.log == nil {
		t.Error("logger should default to slog.Default")
	}
}
