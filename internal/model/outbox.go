package model

import "github.com/google/uuid"

// OutboxEvent is an event waiting in outbox_events to be published. Payload
// is the JSON written in the same transaction as the change it describes.
type OutboxEvent struct {
	ID         uuid.UUID
	RoutingKey string
	Payload    []byte
	Attempts   int
}
