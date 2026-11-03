package model

import (
	"time"

	"github.com/google/uuid"
)

// PaymentStatusApproved is the payment status of a monthly charge that MP
// collected. It is the only one that extends Premium; refunded and
// charged_back take it away.
const (
	PaymentStatusApproved    = "approved"
	PaymentStatusRefunded    = "refunded"
	PaymentStatusChargedBack = "charged_back"
)

// SubscriptionRecord is our copy of a Mercado Pago preapproval. MPStatus is
// stored exactly as MP sends it (note: "cancelled", with two l's).
type SubscriptionRecord struct {
	MPPreapprovalID string
	UserID          uuid.UUID
	MPStatus        string
	AmountCents     int64
	CurrencyID      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Premium is what the rest of Discordia sees. PremiumUntil is nil for users
// that never paid.
type Premium struct {
	UserID        uuid.UUID
	PremiumUntil  *time.Time
	Renews        bool
	Version       int64
	ActiveEmitted bool
	UpdatedAt     time.Time
}

func (p Premium) Active(now time.Time) bool {
	return p.PremiumUntil != nil && p.PremiumUntil.After(now)
}

// Charge is one monthly charge of a subscription (an MP authorized payment).
type Charge struct {
	AuthorizedPaymentID string
	PreapprovalID       string
	// Empty until MP attempts the payment.
	PaymentID     string
	Status        string
	PaymentStatus string
	AmountCents   int64
	DebitDate     *time.Time
	RetryAttempt  int
	AppliedAt     *time.Time
}

// Revocation tells the caller which preapproval to cancel in MP after a
// charge was refunded or charged back.
type Revocation struct {
	UserID        uuid.UUID
	PreapprovalID string
}
