package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/events-contract/boosts"
	"github.com/discordia-grupo01/events-contract/servers"

	"github.com/discordia-grupo01/billing/internal/model"
)

const (
	// BoostCostPoints is the group's decision, documented for the HU.
	BoostCostPoints = 100

	// ServersEventsQueue is billing's queue for what servers publishes.
	ServersEventsQueue = "billing.servers_events"

	boostRepublishAfter = 5 * time.Minute
	boostCancelAfter    = 24 * time.Hour
	boostBatchSize      = 100
	defaultBoostLimit   = 50
	maxBoostLimit       = 100
)

func ServersEventsRoutingKeys() []string {
	return []string{boosts.RoutingKeyApplied, boosts.RoutingKeyRejected, servers.RoutingKeyServerDeleted}
}

type BoostStore interface {
	Create(ctx context.Context, b model.Boost, idempotencyKey string) (model.Boost, bool, error)
	Get(ctx context.Context, id uuid.UUID) (model.Boost, error)
	ListForUser(ctx context.Context, userID uuid.UUID, limit int) ([]model.Boost, error)
	MarkApplied(ctx context.Context, id uuid.UUID) (bool, error)
	Reject(ctx context.Context, id uuid.UUID, detail string) (bool, error)
	Remove(ctx context.Context, userID, id uuid.UUID) (model.Boost, error)
	RemoveForServer(ctx context.Context, serverID uuid.UUID) (int, error)
	RepublishPending(ctx context.Context, before time.Time, limit int) (int, error)
	CancelExpired(ctx context.Context, before time.Time, limit int) (int, error)
}

// BoostService is billing's side of the boost saga: it reserves the points,
// servers decides, and a rejection gives the points back.
type BoostService struct {
	store  BoostStore
	secret string
	log    *slog.Logger
	now    func() time.Time
}

func NewBoostService(store BoostStore, secret string, logger *slog.Logger) *BoostService {
	if logger == nil {
		logger = slog.Default()
	}
	return &BoostService{store: store, secret: secret, log: logger, now: time.Now}
}

// Request starts a boost. The answer is pending: the client polls Get until
// servers decides. Without a key, every call is a new boost.
func (s *BoostService) Request(ctx context.Context, userID, serverID uuid.UUID, idempotencyKey string) (model.Boost, error) {
	boost, created, err := s.store.Create(ctx, model.Boost{
		ID:       uuid.New(),
		UserID:   userID,
		ServerID: serverID,
		Points:   BoostCostPoints,
	}, idempotencyKey)
	if err != nil {
		return model.Boost{}, err
	}
	if !created && boost.ServerID != serverID {
		return model.Boost{}, ErrCheckoutKeyReused
	}
	return boost, nil
}

// Get hides other users' boosts behind ErrNotFound.
func (s *BoostService) Get(ctx context.Context, userID, id uuid.UUID) (model.Boost, error) {
	boost, err := s.store.Get(ctx, id)
	if err != nil {
		return model.Boost{}, err
	}
	if boost.UserID != userID {
		return model.Boost{}, model.ErrNotFound
	}
	return boost, nil
}

func (s *BoostService) List(ctx context.Context, userID uuid.UUID, limit int) ([]model.Boost, error) {
	if limit <= 0 {
		limit = defaultBoostLimit
	}
	return s.store.ListForUser(ctx, userID, min(limit, maxBoostLimit))
}

func (s *BoostService) Remove(ctx context.Context, userID, id uuid.UUID) (model.Boost, error) {
	return s.store.Remove(ctx, userID, id)
}

func (s *BoostService) RepublishPending(ctx context.Context) (int, error) {
	return s.store.RepublishPending(ctx, s.now().Add(-boostRepublishAfter), boostBatchSize)
}

func (s *BoostService) CancelExpired(ctx context.Context) (int, error) {
	return s.store.CancelExpired(ctx, s.now().Add(-boostCancelAfter), boostBatchSize)
}

// HandleEvent processes what servers publishes. It returns an error only for
// what can work on a retry, like the database failing: a bad event is logged and
// dropped, because requeueing it would loop forever. Never reject on a technical
// failure: that would give points back for a boost that may be fine.
func (s *BoostService) HandleEvent(ctx context.Context, routingKey string, body []byte) error {
	switch routingKey {
	case boosts.RoutingKeyApplied, boosts.RoutingKeyRejected:
		return s.handleAnswer(ctx, routingKey, body)
	case servers.RoutingKeyServerDeleted:
		return s.handleServerDeleted(ctx, body)
	default:
		s.log.Warn("boost event dropped", "reason", "unknown_routing_key", "routing_key", routingKey)
		return nil
	}
}

func (s *BoostService) handleAnswer(ctx context.Context, routingKey string, body []byte) error {
	var evt boosts.SagaEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		s.log.Warn("boost event dropped", "reason", "malformed", "routing_key", routingKey)
		return nil
	}
	if !evt.Verify(routingKey, s.secret) {
		s.log.Warn("boost event dropped", "reason", "invalid_signature", "routing_key", routingKey, "event_id", evt.EventID)
		return nil
	}
	id, err := uuid.Parse(evt.BoostID)
	if err != nil {
		s.log.Warn("boost event dropped", "reason", "invalid_boost_id", "event_id", evt.EventID)
		return nil
	}

	if routingKey == boosts.RoutingKeyApplied {
		if _, err := s.store.MarkApplied(ctx, id); err != nil {
			return fmt.Errorf("service: boost applied: %w", err)
		}
		return nil
	}
	if _, err := s.store.Reject(ctx, id, evt.Reason); err != nil {
		return fmt.Errorf("service: boost rejected: %w", err)
	}
	return nil
}

// A forged servers.server_deleted, which isn't signed, could mark boosts as
// removed but gives no points or boosts away.
func (s *BoostService) handleServerDeleted(ctx context.Context, body []byte) error {
	var evt servers.ServerDeleted
	if err := json.Unmarshal(body, &evt); err != nil {
		s.log.Warn("boost event dropped", "reason", "malformed", "routing_key", servers.RoutingKeyServerDeleted)
		return nil
	}
	serverID, err := uuid.Parse(evt.ServerID)
	if err != nil {
		s.log.Warn("boost event dropped", "reason", "invalid_server_id", "event_id", evt.EventID)
		return nil
	}
	if _, err := s.store.RemoveForServer(ctx, serverID); err != nil {
		return fmt.Errorf("service: server deleted: %w", err)
	}
	return nil
}
