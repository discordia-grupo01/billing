package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

// What Premium costs. Like pointsPackages, this is the only place to change
// it; it is sent to MP in pesos and stored in cents.
const (
	// A checkout the user never finished is cancelled in MP after this, so
	// its link stops working and the job stops asking about it.
	subscriptionAbandonAfter = 7 * 24 * time.Hour
	reconcileBatch           = 500
	expireBatch              = 100
)

const (
	premiumPricePesos = 4999
	premiumCurrency   = "ARS"
	premiumReason     = "Discordia Premium"
)

var (
	// ErrAlreadySubscribed means the user has a subscription that MP keeps
	// charging, so a new checkout would charge them twice.
	ErrAlreadySubscribed  = errors.New("user already has a live subscription")
	ErrNoLiveSubscription = errors.New("user has no subscription to cancel")
	ErrPayerEmailRequired = errors.New("payer email is required")
	// ErrSubscriptionMismatch means MP's subscription belongs to another
	// user than ours. Like ErrOrderMismatch, retrying won't fix it.
	ErrSubscriptionMismatch = errors.New("subscription does not match its owner")
)

// What the clients show; computed here so web and mobile don't have to
// combine MP statuses with dates.
type PremiumState string

const (
	PremiumStateNone                   PremiumState = "none"
	PremiumStateProcessingFirstPayment PremiumState = "processing_first_payment"
	PremiumStateActive                 PremiumState = "active"
	PremiumStateActiveCanceled         PremiumState = "active_canceled"
	PremiumStateExpired                PremiumState = "expired"
)

type PremiumView struct {
	State        PremiumState
	PremiumUntil *time.Time
	Renews       bool
	// MPStatus of the latest subscription, empty if the user never started one.
	MPStatus string
}

// PremiumStore is the slice of the premium repository this service needs.
type PremiumStore interface {
	CreateSubscription(ctx context.Context, s model.SubscriptionRecord) (model.SubscriptionRecord, error)
	GetSubscription(ctx context.Context, preapprovalID string) (model.SubscriptionRecord, error)
	ListSubscriptionsToReconcile(ctx context.Context, limit int) ([]model.SubscriptionRecord, error)
	ExpireDue(ctx context.Context, limit int) (int, error)
	LatestSubscription(ctx context.Context, userID uuid.UUID) (model.SubscriptionRecord, error)
	SyncSubscriptionStatus(ctx context.Context, preapprovalID, mpStatus string) (bool, error)
	ApplyCharge(ctx context.Context, c model.Charge) (bool, error)
	RevokeCharge(ctx context.Context, paymentID, paymentStatus string) (model.Revocation, bool, error)
	GetPremium(ctx context.Context, userID uuid.UUID) (model.Premium, error)
	HasAppliedCharge(ctx context.Context, preapprovalID string) (bool, error)
}

type PremiumConfig struct {
	// Where MP sends the user back after authorizing the subscription.
	BackURL string
	// Sandbox only: MP rejects subscriptions whose payer is not the test
	// buyer, so this replaces the user's email when set.
	TestPayerEmail string
}

type PremiumService struct {
	store   PremiumStore
	gateway PaymentGateway
	cfg     PremiumConfig
	log     *slog.Logger
	now     func() time.Time
}

func NewPremiumService(store PremiumStore, gateway PaymentGateway, cfg PremiumConfig, logger *slog.Logger) *PremiumService {
	if logger == nil {
		logger = slog.Default()
	}
	return &PremiumService{store: store, gateway: gateway, cfg: cfg, log: logger, now: time.Now}
}

func isLive(mpStatus string) bool {
	return mpStatus == model.SubscriptionStatusAuthorized || mpStatus == model.SubscriptionStatusPaused
}

// StartCheckout creates a pending subscription and returns the MP page where
// the user enters the card. Premium starts with the first approved charge,
// not here.
func (s *PremiumService) StartCheckout(ctx context.Context, userID uuid.UUID, payerEmail string) (string, error) {
	latest, err := s.store.LatestSubscription(ctx, userID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return "", err
	}
	if err == nil && isLive(latest.MPStatus) {
		return "", ErrAlreadySubscribed
	}

	if s.cfg.TestPayerEmail != "" {
		payerEmail = s.cfg.TestPayerEmail
	}
	if payerEmail == "" {
		return "", ErrPayerEmailRequired
	}
	sub, err := s.gateway.CreateSubscription(ctx, model.SubscriptionRequest{
		ExternalReference: userID.String(),
		PayerEmail:        payerEmail,
		Reason:            premiumReason,
		AmountCents:       premiumPricePesos * 100,
		CurrencyID:        premiumCurrency,
		BackURL:           s.cfg.BackURL,
	})
	if err != nil {
		return "", err
	}
	if _, err := s.store.CreateSubscription(ctx, model.SubscriptionRecord{
		MPPreapprovalID: sub.ID,
		UserID:          userID,
		MPStatus:        sub.Status,
		AmountCents:     premiumPricePesos * 100,
		CurrencyID:      premiumCurrency,
	}); err != nil {
		return "", err
	}
	return sub.InitPoint, nil
}

// Cancel stops the renewal. The user keeps Premium until premium_until: they
// already paid for that month.
func (s *PremiumService) Cancel(ctx context.Context, userID uuid.UUID) (PremiumView, error) {
	latest, err := s.store.LatestSubscription(ctx, userID)
	if errors.Is(err, model.ErrNotFound) {
		return PremiumView{}, ErrNoLiveSubscription
	}
	if err != nil {
		return PremiumView{}, err
	}
	if !isLive(latest.MPStatus) {
		return PremiumView{}, ErrNoLiveSubscription
	}

	sub, err := s.gateway.CancelSubscription(ctx, latest.MPPreapprovalID)
	if err != nil {
		return PremiumView{}, err
	}
	if _, err := s.store.SyncSubscriptionStatus(ctx, latest.MPPreapprovalID, sub.Status); err != nil {
		return PremiumView{}, err
	}
	return s.view(ctx, userID)
}

// State returns what the client should show. With refresh it first asks MP
// for the latest state, for the return page that polls while the webhook may
// not have arrived yet. Only the latest subscription is refreshed.
func (s *PremiumService) State(ctx context.Context, userID uuid.UUID, refresh bool) (PremiumView, error) {
	if refresh {
		latest, err := s.store.LatestSubscription(ctx, userID)
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			return PremiumView{}, err
		}
		if err == nil && (latest.MPStatus == model.SubscriptionStatusPending || isLive(latest.MPStatus)) {
			// The page keeps polling: show what we have instead of failing.
			if err := s.ReconcileSubscription(ctx, latest.MPPreapprovalID); err != nil {
				s.log.Warn("premium refresh failed", "preapproval_id", latest.MPPreapprovalID, "error", err)
			}
		}
	}
	return s.view(ctx, userID)
}

func (s *PremiumService) view(ctx context.Context, userID uuid.UUID) (PremiumView, error) {
	premium, err := s.store.GetPremium(ctx, userID)
	if err != nil {
		return PremiumView{}, err
	}
	v := PremiumView{PremiumUntil: premium.PremiumUntil, Renews: premium.Renews}

	latest, err := s.store.LatestSubscription(ctx, userID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return PremiumView{}, err
	}
	hasLatest := err == nil
	if hasLatest {
		v.MPStatus = latest.MPStatus
	}

	now := s.now()
	switch {
	case premium.Active(now) && premium.Renews:
		v.State = PremiumStateActive
	case premium.Active(now):
		v.State = PremiumStateActiveCanceled
	default:
		waiting := false
		if hasLatest && latest.MPStatus == model.SubscriptionStatusAuthorized {
			applied, err := s.store.HasAppliedCharge(ctx, latest.MPPreapprovalID)
			if err != nil {
				return PremiumView{}, err
			}
			waiting = !applied
		}
		switch {
		case waiting:
			v.State = PremiumStateProcessingFirstPayment
		case premium.PremiumUntil != nil:
			v.State = PremiumStateExpired
		default:
			v.State = PremiumStateNone
		}
	}
	return v, nil
}

// ProcessSubscriptionNotification handles subscription_preapproval: the
// subscription was authorized, paused or cancelled.
func (s *PremiumService) ProcessSubscriptionNotification(ctx context.Context, preapprovalID string) error {
	err := s.syncStatus(ctx, preapprovalID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, model.ErrNotFound) {
		s.log.Info("ignoring notification for unknown subscription", "preapproval_id", preapprovalID)
		return nil
	}
	return err
}

// ProcessChargeNotification handles subscription_authorized_payment: a
// monthly charge was created, attempted or retried.
func (s *PremiumService) ProcessChargeNotification(ctx context.Context, chargeID string) error {
	charge, err := s.gateway.GetSubscriptionCharge(ctx, chargeID)
	if errors.Is(err, ErrNotFound) {
		s.log.Info("ignoring notification for unknown charge", "charge_id", chargeID)
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.store.ApplyCharge(ctx, charge)
	if errors.Is(err, model.ErrNotFound) {
		s.log.Warn("ignoring charge of unknown subscription", "charge_id", chargeID, "preapproval_id", charge.PreapprovalID)
		return nil
	}
	return err
}

// ProcessChargebackNotification handles topic_chargebacks_wh, which carries
// the chargeback id: the disputed payment goes through
// ProcessPaymentNotification.
func (s *PremiumService) ProcessChargebackNotification(ctx context.Context, chargebackID string) error {
	paymentID, err := s.gateway.GetChargebackPaymentID(ctx, chargebackID)
	if errors.Is(err, ErrNotFound) {
		s.log.Info("ignoring notification for unknown chargeback", "chargeback_id", chargebackID)
		return nil
	}
	if err != nil {
		return err
	}
	return s.ProcessPaymentNotification(ctx, paymentID)
}

// ProcessPaymentNotification handles payment notifications and the payments
// behind chargebacks. Only refunds and chargebacks of monthly charges matter:
// Premium ends at once and the subscription is cancelled in MP. Any
// chargeback detail counts, same as with points.
func (s *PremiumService) ProcessPaymentNotification(ctx context.Context, paymentID string) error {
	payment, err := s.gateway.GetPayment(ctx, paymentID)
	if errors.Is(err, ErrNotFound) {
		s.log.Info("ignoring notification for unknown payment", "payment_id", paymentID)
		return nil
	}
	if err != nil {
		return err
	}
	if payment.Status != model.PaymentStatusRefunded && payment.Status != model.PaymentStatusChargedBack {
		return nil
	}

	rev, _, err := s.store.RevokeCharge(ctx, payment.ID, payment.Status)
	if errors.Is(err, model.ErrNotFound) {
		// Not a monthly charge: points payments arrive through the order topic.
		return nil
	}
	if err != nil {
		return err
	}
	// Checked on every run, not only when RevokeCharge revoked something, so
	// a retry still cancels if the previous attempt failed after revoking.
	return s.cancelIfLive(ctx, rev.PreapprovalID)
}

// ExpireDue tells the rest of Discordia that Premium ended for users whose
// premium_until passed (failed renewal or end of a cancelled period).
func (s *PremiumService) ExpireDue(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.store.ExpireDue(ctx, expireBatch)
		total += n
		if err != nil || n < expireBatch {
			return total, err
		}
	}
}

// ReconcileSubscriptions runs ReconcileSubscription on every subscription MP
// may still change, so Premium is right even if webhooks never arrive.
// Checkouts abandoned for a week are cancelled. Errors on one subscription
// are logged and don't stop the rest; it returns how many it looked at.
func (s *PremiumService) ReconcileSubscriptions(ctx context.Context) (int, error) {
	subs, err := s.store.ListSubscriptionsToReconcile(ctx, reconcileBatch)
	if err != nil {
		return 0, err
	}
	abandonedBefore := s.now().Add(-subscriptionAbandonAfter)
	for _, sub := range subs {
		log := s.log.With("preapproval_id", sub.MPPreapprovalID, "user_id", sub.UserID)
		if err := s.ReconcileSubscription(ctx, sub.MPPreapprovalID); err != nil {
			log.Error("could not reconcile subscription", "error", err)
			continue
		}
		if sub.CreatedAt.Before(abandonedBefore) {
			if err := s.cancelIfPending(ctx, sub.MPPreapprovalID); err != nil {
				log.Error("could not cancel abandoned subscription", "error", err)
			}
		}
	}
	return len(subs), nil
}

// cancelIfPending cancels a subscription the user never authorized. The
// status is read again because the reconcile that just ran may have changed it.
func (s *PremiumService) cancelIfPending(ctx context.Context, preapprovalID string) error {
	record, err := s.store.GetSubscription(ctx, preapprovalID)
	if err != nil || record.MPStatus != model.SubscriptionStatusPending {
		return err
	}
	sub, err := s.gateway.CancelSubscription(ctx, preapprovalID)
	if err != nil {
		return err
	}
	s.log.Info("cancelled abandoned subscription checkout", "preapproval_id", preapprovalID, "user_id", record.UserID)
	_, err = s.store.SyncSubscriptionStatus(ctx, preapprovalID, sub.Status)
	return err
}

// ReconcileSubscription brings one subscription up to date with MP: its
// status and every charge. Used by the refresh and by the reconciliation job,
// so Premium activates even if a webhook never arrives.
func (s *PremiumService) ReconcileSubscription(ctx context.Context, preapprovalID string) error {
	if err := s.syncStatus(ctx, preapprovalID); err != nil {
		return err
	}
	charges, err := s.gateway.ListSubscriptionCharges(ctx, preapprovalID)
	if err != nil {
		return err
	}
	for _, c := range charges {
		if _, err := s.store.ApplyCharge(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// syncStatus re-fetches the subscription and stores its status. If the user
// authorized a second subscription while another one is live, the new one is
// cancelled in MP so they are never charged twice.
func (s *PremiumService) syncStatus(ctx context.Context, preapprovalID string) error {
	record, err := s.store.GetSubscription(ctx, preapprovalID)
	if err != nil {
		return err
	}
	sub, err := s.gateway.GetSubscription(ctx, preapprovalID)
	if err != nil {
		return err
	}
	if sub.ExternalReference != record.UserID.String() {
		return fmt.Errorf("%w: subscription %s is ours for user %s, MP says %q", ErrSubscriptionMismatch,
			preapprovalID, record.UserID, sub.ExternalReference)
	}

	_, err = s.store.SyncSubscriptionStatus(ctx, preapprovalID, sub.Status)
	if !errors.Is(err, model.ErrLiveSubscriptionExists) {
		return err
	}
	s.log.Warn("cancelling duplicate subscription", "preapproval_id", preapprovalID, "user_id", record.UserID)
	cancelled, err := s.gateway.CancelSubscription(ctx, preapprovalID)
	if err != nil {
		return err
	}
	_, err = s.store.SyncSubscriptionStatus(ctx, preapprovalID, cancelled.Status)
	return err
}

func (s *PremiumService) cancelIfLive(ctx context.Context, preapprovalID string) error {
	record, err := s.store.GetSubscription(ctx, preapprovalID)
	if err != nil {
		return err
	}
	if !isLive(record.MPStatus) {
		return nil
	}
	sub, err := s.gateway.CancelSubscription(ctx, preapprovalID)
	if err != nil {
		return err
	}
	_, err = s.store.SyncSubscriptionStatus(ctx, preapprovalID, sub.Status)
	return err
}
