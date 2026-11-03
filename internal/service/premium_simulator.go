package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

// Mercado Pago can't move time forward or make a renewal fail on demand, so
// renewals (HU CA3) and failed renewals (CA4) can't be shown with real
// payments. The simulator runs the same code a real charge or the expiry job
// would. It only exists when BILLING_DEV_TOOLS=true.

var ErrNoActivePremium = errors.New("user has no active premium")

// SimulatorStore is the slice of the premium repository the simulator needs.
type SimulatorStore interface {
	LatestSubscription(ctx context.Context, userID uuid.UUID) (model.SubscriptionRecord, error)
	ApplyCharge(ctx context.Context, c model.Charge) (bool, error)
	EndPremiumPeriod(ctx context.Context, userID uuid.UUID) (bool, error)
}

type PremiumSimulator struct {
	premium *PremiumService
	store   SimulatorStore
	newID   func() uuid.UUID
}

func NewPremiumSimulator(premium *PremiumService, store SimulatorStore) *PremiumSimulator {
	return &PremiumSimulator{premium: premium, store: store, newID: uuid.New}
}

// SimulateCharge records a monthly charge of the user's live subscription,
// as if MP had just tried to collect it. An approved one extends Premium a
// month from premium_until, with no gap; a rejected one changes nothing, and
// Premium ends when premium_until passes (see SimulateExpiry).
func (s *PremiumSimulator) SimulateCharge(ctx context.Context, userID uuid.UUID, approved bool) (PremiumView, error) {
	latest, err := s.store.LatestSubscription(ctx, userID)
	if errors.Is(err, model.ErrNotFound) || (err == nil && !isLive(latest.MPStatus)) {
		return PremiumView{}, ErrNoLiveSubscription
	}
	if err != nil {
		return PremiumView{}, err
	}

	now := s.premium.now()
	charge := model.Charge{
		AuthorizedPaymentID: "sim-" + s.newID().String(),
		PreapprovalID:       latest.MPPreapprovalID,
		Status:              "processed",
		PaymentStatus:       model.PaymentStatusApproved,
		AmountCents:         latest.AmountCents,
		DebitDate:           &now,
	}
	if !approved {
		charge.Status, charge.PaymentStatus = "recycling", "rejected"
	}
	if _, err := s.store.ApplyCharge(ctx, charge); err != nil {
		return PremiumView{}, err
	}
	return s.premium.view(ctx, userID)
}

// SimulateExpiry moves premium_until to now, as if the paid period had just
// ended, and runs the expiry job, which tells the rest of Discordia.
func (s *PremiumSimulator) SimulateExpiry(ctx context.Context, userID uuid.UUID) (PremiumView, error) {
	ended, err := s.store.EndPremiumPeriod(ctx, userID)
	if err != nil {
		return PremiumView{}, err
	}
	if !ended {
		return PremiumView{}, ErrNoActivePremium
	}
	if _, err := s.premium.ExpireDue(ctx); err != nil {
		return PremiumView{}, err
	}
	return s.premium.view(ctx, userID)
}
