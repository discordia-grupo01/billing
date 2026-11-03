package service

import (
	"context"
	"errors"
	"fmt"
	"time"

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

	subReq    model.SubscriptionRequest
	sub       model.Subscription
	cancelled []string
	charges   []model.Charge
	payment   model.Payment
	failOn    map[string]error
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

func (g *fakeGateway) CreateSubscription(_ context.Context, req model.SubscriptionRequest) (model.Subscription, error) {
	g.subReq = req
	if err := g.failOn["CreateSubscription"]; err != nil {
		return model.Subscription{}, err
	}
	return g.sub, nil
}

func (g *fakeGateway) GetSubscription(_ context.Context, id string) (model.Subscription, error) {
	if err := g.failOn["GetSubscription"]; err != nil {
		return model.Subscription{}, err
	}
	if g.sub.ID != id {
		return model.Subscription{}, fmt.Errorf("preapproval %s: %w", id, ErrNotFound)
	}
	return g.sub, nil
}

func (g *fakeGateway) CancelSubscription(_ context.Context, id string) (model.Subscription, error) {
	g.cancelled = append(g.cancelled, id)
	if err := g.failOn["CancelSubscription"]; err != nil {
		return model.Subscription{}, err
	}
	if g.sub.ID == id {
		g.sub.Status = model.SubscriptionStatusCancelled
	}
	return model.Subscription{ID: id, Status: model.SubscriptionStatusCancelled}, nil
}

func (g *fakeGateway) GetSubscriptionCharge(_ context.Context, id string) (model.Charge, error) {
	if err := g.failOn["GetSubscriptionCharge"]; err != nil {
		return model.Charge{}, err
	}
	for _, c := range g.charges {
		if c.AuthorizedPaymentID == id {
			return c, nil
		}
	}
	return model.Charge{}, fmt.Errorf("authorized payment %s: %w", id, ErrNotFound)
}

func (g *fakeGateway) ListSubscriptionCharges(_ context.Context, id string) ([]model.Charge, error) {
	if err := g.failOn["ListSubscriptionCharges"]; err != nil {
		return nil, err
	}
	var out []model.Charge
	for _, c := range g.charges {
		if c.PreapprovalID == id {
			out = append(out, c)
		}
	}
	return out, nil
}

func (g *fakeGateway) GetPayment(_ context.Context, id string) (model.Payment, error) {
	if err := g.failOn["GetPayment"]; err != nil {
		return model.Payment{}, err
	}
	if g.payment.ID != id {
		return model.Payment{}, fmt.Errorf("payment %s: %w", id, ErrNotFound)
	}
	return g.payment, nil
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

// fakePremiumStore mimics the real premium repository in memory, including
// the one-live-subscription-per-user rule and the charge bookkeeping.
type fakePremiumStore struct {
	subs    []model.SubscriptionRecord // in creation order
	premium map[uuid.UUID]model.Premium
	charges map[string]model.Charge
	now     time.Time
	failOn  map[string]error
}

func newFakePremiumStore(now time.Time) *fakePremiumStore {
	return &fakePremiumStore{premium: map[uuid.UUID]model.Premium{}, charges: map[string]model.Charge{}, now: now}
}

func (f *fakePremiumStore) sub(id string) (int, bool) {
	for i, s := range f.subs {
		if s.MPPreapprovalID == id {
			return i, true
		}
	}
	return 0, false
}

func (f *fakePremiumStore) otherLive(userID uuid.UUID, exceptID string) bool {
	for _, s := range f.subs {
		if s.UserID == userID && s.MPPreapprovalID != exceptID &&
			(s.MPStatus == model.SubscriptionStatusAuthorized || s.MPStatus == model.SubscriptionStatusPaused) {
			return true
		}
	}
	return false
}

func (f *fakePremiumStore) CreateSubscription(_ context.Context, s model.SubscriptionRecord) (model.SubscriptionRecord, error) {
	if err := f.failOn["CreateSubscription"]; err != nil {
		return model.SubscriptionRecord{}, err
	}
	f.subs = append(f.subs, s)
	return s, nil
}

func (f *fakePremiumStore) GetSubscription(_ context.Context, id string) (model.SubscriptionRecord, error) {
	if err := f.failOn["GetSubscription"]; err != nil {
		return model.SubscriptionRecord{}, err
	}
	i, ok := f.sub(id)
	if !ok {
		return model.SubscriptionRecord{}, model.ErrNotFound
	}
	return f.subs[i], nil
}

func (f *fakePremiumStore) LatestSubscription(_ context.Context, userID uuid.UUID) (model.SubscriptionRecord, error) {
	if err := f.failOn["LatestSubscription"]; err != nil {
		return model.SubscriptionRecord{}, err
	}
	for i := len(f.subs) - 1; i >= 0; i-- {
		if f.subs[i].UserID == userID {
			return f.subs[i], nil
		}
	}
	return model.SubscriptionRecord{}, model.ErrNotFound
}

func (f *fakePremiumStore) SyncSubscriptionStatus(_ context.Context, id, mpStatus string) (bool, error) {
	if err := f.failOn["SyncSubscriptionStatus"]; err != nil {
		return false, err
	}
	i, ok := f.sub(id)
	if !ok {
		return false, model.ErrNotFound
	}
	userID := f.subs[i].UserID
	live := mpStatus == model.SubscriptionStatusAuthorized || mpStatus == model.SubscriptionStatusPaused
	if live && f.otherLive(userID, id) {
		return false, model.ErrLiveSubscriptionExists
	}
	f.subs[i].MPStatus = mpStatus

	renews := false
	for _, s := range f.subs {
		if s.UserID == userID && s.MPStatus == model.SubscriptionStatusAuthorized {
			renews = true
		}
	}
	p := f.premium[userID]
	p.UserID = userID
	changed := p.Renews != renews
	p.Renews = renews
	f.premium[userID] = p
	return changed, nil
}

func (f *fakePremiumStore) ApplyCharge(_ context.Context, c model.Charge) (bool, error) {
	if err := f.failOn["ApplyCharge"]; err != nil {
		return false, err
	}
	i, ok := f.sub(c.PreapprovalID)
	if !ok {
		return false, model.ErrNotFound
	}
	prev, seen := f.charges[c.AuthorizedPaymentID]
	if seen {
		c.AppliedAt = prev.AppliedAt
	}
	f.charges[c.AuthorizedPaymentID] = c
	if c.AppliedAt != nil || c.PaymentStatus != model.PaymentStatusApproved {
		return false, nil
	}

	userID := f.subs[i].UserID
	from := f.now
	if c.DebitDate != nil {
		from = *c.DebitDate
	}
	p := f.premium[userID]
	p.UserID = userID
	if p.PremiumUntil != nil && p.PremiumUntil.After(from) {
		from = *p.PremiumUntil
	}
	until := from.AddDate(0, 1, 0)
	p.PremiumUntil = &until
	f.premium[userID] = p
	applied := f.now
	c.AppliedAt = &applied
	f.charges[c.AuthorizedPaymentID] = c
	return true, nil
}

func (f *fakePremiumStore) RevokeCharge(_ context.Context, paymentID, paymentStatus string) (model.Revocation, bool, error) {
	if err := f.failOn["RevokeCharge"]; err != nil {
		return model.Revocation{}, false, err
	}
	for id, c := range f.charges {
		if c.PaymentID != paymentID {
			continue
		}
		i, _ := f.sub(c.PreapprovalID)
		rev := model.Revocation{UserID: f.subs[i].UserID, PreapprovalID: c.PreapprovalID}
		if c.PaymentStatus == paymentStatus {
			return rev, false, nil
		}
		c.PaymentStatus = paymentStatus
		f.charges[id] = c
		if c.AppliedAt == nil {
			return rev, false, nil
		}
		p := f.premium[rev.UserID]
		if p.PremiumUntil != nil && p.PremiumUntil.After(f.now) {
			now := f.now
			p.PremiumUntil = &now
		}
		p.Renews = false
		f.premium[rev.UserID] = p
		return rev, true, nil
	}
	return model.Revocation{}, false, model.ErrNotFound
}

func (f *fakePremiumStore) GetPremium(_ context.Context, userID uuid.UUID) (model.Premium, error) {
	if err := f.failOn["GetPremium"]; err != nil {
		return model.Premium{}, err
	}
	p, ok := f.premium[userID]
	if !ok {
		return model.Premium{UserID: userID}, nil
	}
	return p, nil
}

func (f *fakePremiumStore) HasAppliedCharge(_ context.Context, id string) (bool, error) {
	if err := f.failOn["HasAppliedCharge"]; err != nil {
		return false, err
	}
	for _, c := range f.charges {
		if c.PreapprovalID == id && c.AppliedAt != nil {
			return true, nil
		}
	}
	return false, nil
}
