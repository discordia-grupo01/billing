package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type BoostStatus string

const (
	BoostStatusPending BoostStatus = "pending"
	BoostStatusActive  BoostStatus = "active"
	BoostStatusFailed  BoostStatus = "failed"
	BoostStatusRemoved BoostStatus = "removed"
)

const (
	// BoostDetailNoResponse: servers didn't answer in time.
	BoostDetailNoResponse = "no_response"
	// BoostDetailServerDeleted: the boosted server no longer exists.
	BoostDetailServerDeleted = "server_deleted"
)

var ErrBoostNotActive = errors.New("only an active boost can be removed")

// BoostListParams filters and pages a user's boosts. A nil ServerID lists every
// server.
type BoostListParams struct {
	ServerID *uuid.UUID
	Limit    int
	Offset   int
}

// BoostPage is one page of a user's boosts. Total and Active count every boost
// that matches the filter, not only this page.
type BoostPage struct {
	Boosts []Boost
	Total  int
	Active int
	Limit  int
	Offset int
}

// BoostedServer sums up a user's boosts on one server, in every status.
type BoostedServer struct {
	ServerID    uuid.UUID
	Active      int
	Pending     int
	Failed      int
	Removed     int
	LastBoostAt time.Time
}

// BoostedServerPage is one page of the servers a user boosted.
type BoostedServerPage struct {
	Servers []BoostedServer
	Total   int
	Limit   int
	Offset  int
}

type Boost struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	ServerID uuid.UUID
	Points   int
	Status   BoostStatus
	// Why it failed or was removed; empty otherwise.
	StatusDetail string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type InsufficientPointsError struct {
	Balance int64
	Cost    int64
}

func (e InsufficientPointsError) Error() string {
	return fmt.Sprintf("insufficient points: %d of %d", e.Balance, e.Cost)
}

func (e InsufficientPointsError) Missing() int64 { return e.Cost - e.Balance }
