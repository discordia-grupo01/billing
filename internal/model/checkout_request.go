package model

import "github.com/google/uuid"

type CheckoutScope string

const (
	CheckoutScopePoints  CheckoutScope = "points"
	CheckoutScopePremium CheckoutScope = "premium"
)

// CheckoutKey identifies a request: the same key from another user or for
// another product is a different request.
type CheckoutKey struct {
	UserID uuid.UUID
	Scope  CheckoutScope
	Key    string
}

// CheckoutReservation is the result of reserving a key. If Acquired, the caller
// runs the checkout. If not, the fields describe the earlier request, and an
// empty CheckoutURL means it is still in progress.
type CheckoutReservation struct {
	Acquired    bool
	Fingerprint string
	PurchaseID  uuid.UUID
	CheckoutURL string
}
