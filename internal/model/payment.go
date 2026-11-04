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

// Order statuses of the Orders API. Note "canceled" with one l, unlike
// preapprovals and payments.
const (
	OrderStatusCreated        = "created"
	OrderStatusProcessing     = "processing"
	OrderStatusActionRequired = "action_required"
	OrderStatusProcessed      = "processed"
	OrderStatusFailed         = "failed"
	OrderStatusCanceled       = "canceled"
	OrderStatusRefunded       = "refunded"
	OrderStatusChargedBack    = "charged_back"

	OrderDetailExpired           = "expired"
	OrderDetailPartiallyRefunded = "partially_refunded"

	// Status of a payment attempt inside an order.
	OrderPaymentStatusFailed = "failed"
)

type PointsOrder struct {
	ID                string
	Status            string
	StatusDetail      string
	ExternalReference string
	AmountCents       int64
	// Only returned on creation.
	CheckoutURL string
	// The latest payment attempt, empty if the buyer hasn't tried to pay.
	// A rejected card leaves the order open (the buyer can try another one),
	// so only this tells that the attempt failed.
	LastPaymentStatus       string
	LastPaymentStatusDetail string
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

// Payment is a single MP payment. Subscription charges reach us as payment
// notifications too, e.g. when one is refunded or charged back.
type Payment struct {
	ID                string
	Status            string
	StatusDetail      string
	ExternalReference string
	AmountCents       int64
}
