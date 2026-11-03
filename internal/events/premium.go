package events

import "time"

const RoutingKeyPremiumUpdated = "billing.premium_updated"

// Meta mirrors envelope.Meta from events-contract.
type Meta struct {
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// PremiumUpdated is a full snapshot of a user's Premium. Consumers apply it
// only if Version is higher than the one they have, so duplicates and
// out-of-order deliveries are harmless.
type PremiumUpdated struct {
	Meta
	UserID       string     `json:"user_id"`
	Active       bool       `json:"active"`
	PremiumUntil *time.Time `json:"premium_until"`
	Renews       bool       `json:"renews"`
	Version      int64      `json:"version"`
}
