package model

// Amounts are in cents to avoid float rounding; the gateway adapter converts
// them to whatever format the provider expects.

type PointsOrderRequest struct {
	// IdempotencyKey must stay the same across retries of the same purchase
	// attempt, so the provider doesn't create the order twice.
	IdempotencyKey    string
	ExternalReference string
	PayerEmail        string
	PayerFirstName    string
	PayerLastName     string
	// ItemID is our package id; MP uses it with the title and description
	// for fraud prevention, which affects the approval rate.
	ItemID      string
	Title       string
	Description string
	AmountCents int64
	// Where MP sends the buyer back. Must be public URLs, never localhost.
	SuccessURL string
	FailureURL string
	PendingURL string
}

type PointsOrder struct {
	ID          string
	Status      string
	CheckoutURL string
}

type SubscriptionRequest struct {
	ExternalReference string
	PayerEmail        string
	Reason            string
	AmountCents       int64
	CurrencyID        string
	BackURL           string
}

const (
	SubscriptionStatusPending    = "pending"
	SubscriptionStatusAuthorized = "authorized"
	SubscriptionStatusPaused     = "paused"
	SubscriptionStatusCancelled  = "cancelled"
)

type Subscription struct {
	ID                string
	Status            string
	ExternalReference string
	// InitPoint is where the user goes to authorize the subscription; only
	// returned on creation.
	InitPoint string
}
