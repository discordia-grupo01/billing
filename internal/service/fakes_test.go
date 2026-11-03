package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

var errBoom = errors.New("boom")

// fakeGateway returns canned responses and records the requests it gets.
type fakeGateway struct {
	orderReq  model.PointsOrderRequest
	order     model.PointsOrder
	createErr error
	getErr    error
}

var _ PaymentGateway = (*fakeGateway)(nil)

func (g *fakeGateway) CreatePointsOrder(_ context.Context, req model.PointsOrderRequest) (model.PointsOrder, error) {
	g.orderReq = req
	return g.order, g.createErr
}

func (g *fakeGateway) GetPointsOrder(_ context.Context, orderID string) (model.PointsOrder, error) {
	if g.getErr != nil {
		return model.PointsOrder{}, g.getErr
	}
	if g.order.ID != orderID {
		return model.PointsOrder{}, fmt.Errorf("order %s: %w", orderID, ErrNotFound)
	}
	return g.order, nil
}

func (g *fakeGateway) CreateSubscription(context.Context, model.SubscriptionRequest) (model.Subscription, error) {
	return model.Subscription{}, errors.New("not implemented")
}

func (g *fakeGateway) GetSubscription(context.Context, string) (model.Subscription, error) {
	return model.Subscription{}, errors.New("not implemented")
}

func (g *fakeGateway) CancelSubscription(context.Context, string) (model.Subscription, error) {
	return model.Subscription{}, errors.New("not implemented")
}

func (g *fakeGateway) GetSubscriptionCharge(context.Context, string) (model.Charge, error) {
	return model.Charge{}, errors.New("not implemented")
}

func (g *fakeGateway) ListSubscriptionCharges(context.Context, string) ([]model.Charge, error) {
	return nil, errors.New("not implemented")
}

func (g *fakeGateway) GetPayment(context.Context, string) (model.Payment, error) {
	return model.Payment{}, errors.New("not implemented")
}

// fakePointsStore mimics the transitions of the real repository in memory.
type fakePointsStore struct {
	purchases map[uuid.UUID]model.PointPurchase
	balances  map[uuid.UUID]int64
	ledger    []model.LedgerEntry
	calls     []string
	lastLimit int
	err       error // returned by every method when set
}

func newFakePointsStore() *fakePointsStore {
	return &fakePointsStore{purchases: map[uuid.UUID]model.PointPurchase{}, balances: map[uuid.UUID]int64{}}
}

func (f *fakePointsStore) record(call string) error {
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakePointsStore) CreatePurchase(_ context.Context, p model.PointPurchase) (model.PointPurchase, error) {
	if err := f.record("CreatePurchase"); err != nil {
		return model.PointPurchase{}, err
	}
	p.Status = model.PurchaseStatusPending
	f.purchases[p.ID] = p
	return p, nil
}

func (f *fakePointsStore) SetOrderID(_ context.Context, id uuid.UUID, orderID string) error {
	if err := f.record("SetOrderID"); err != nil {
		return err
	}
	p, ok := f.purchases[id]
	if !ok {
		return model.ErrNotFound
	}
	p.MPOrderID = orderID
	f.purchases[id] = p
	return nil
}

func (f *fakePointsStore) GetPurchase(_ context.Context, id uuid.UUID) (model.PointPurchase, error) {
	if err := f.record("GetPurchase"); err != nil {
		return model.PointPurchase{}, err
	}
	p, ok := f.purchases[id]
	if !ok {
		return model.PointPurchase{}, model.ErrNotFound
	}
	return p, nil
}

func (f *fakePointsStore) ListPurchases(_ context.Context, _ uuid.UUID, limit int) ([]model.PointPurchase, error) {
	f.lastLimit = limit
	return nil, f.record("ListPurchases")
}

func (f *fakePointsStore) MarkUnpaid(_ context.Context, id uuid.UUID, status model.PurchaseStatus, detail string) (bool, error) {
	if err := f.record("MarkUnpaid"); err != nil {
		return false, err
	}
	p := f.purchases[id]
	if p.Status != model.PurchaseStatusPending {
		return false, nil
	}
	p.Status, p.StatusDetail = status, detail
	f.purchases[id] = p
	return true, nil
}

func (f *fakePointsStore) CreditPurchase(_ context.Context, id uuid.UUID, detail string) (bool, error) {
	if err := f.record("CreditPurchase"); err != nil {
		return false, err
	}
	p := f.purchases[id]
	switch p.Status {
	case model.PurchaseStatusPaid, model.PurchaseStatusRefunded, model.PurchaseStatusChargedBack:
		return false, nil
	}
	f.balances[p.UserID] += int64(p.Points)
	f.ledger = append(f.ledger, model.LedgerEntry{UserID: p.UserID, Delta: int64(p.Points), Reason: model.LedgerReasonPurchase, RefID: p.MPOrderID})
	p.Status, p.StatusDetail = model.PurchaseStatusPaid, detail
	f.purchases[id] = p
	return true, nil
}

func (f *fakePointsStore) ReversePurchase(_ context.Context, id uuid.UUID, reason model.LedgerReason) (int64, error) {
	if err := f.record("ReversePurchase"); err != nil {
		return 0, err
	}
	p := f.purchases[id]
	switch p.Status {
	case model.PurchaseStatusRefunded, model.PurchaseStatusChargedBack:
		return 0, nil
	case model.PurchaseStatusPaid:
	default:
		return 0, fmt.Errorf("cannot reverse a %s purchase", p.Status)
	}
	take := min(f.balances[p.UserID], int64(p.Points))
	f.balances[p.UserID] -= take
	f.ledger = append(f.ledger, model.LedgerEntry{UserID: p.UserID, Delta: -take, Reason: reason, RefID: p.MPOrderID})
	p.Status = model.PurchaseStatusRefunded
	if reason == model.LedgerReasonChargeback {
		p.Status = model.PurchaseStatusChargedBack
	}
	p.PointsUnrecovered = p.Points - int(take)
	f.purchases[id] = p
	return take, nil
}

func (f *fakePointsStore) Balance(_ context.Context, userID uuid.UUID) (int64, error) {
	return f.balances[userID], f.record("Balance")
}

func (f *fakePointsStore) ListLedger(_ context.Context, _ uuid.UUID, limit int) ([]model.LedgerEntry, error) {
	f.lastLimit = limit
	return f.ledger, f.record("ListLedger")
}
