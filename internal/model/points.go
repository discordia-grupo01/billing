package model

import (
	"time"

	"github.com/google/uuid"
)

const (
	PurchaseStatusPending     = "pending"
	PurchaseStatusPaid        = "paid"
	PurchaseStatusFailed      = "failed"
	PurchaseStatusExpired     = "expired"
	PurchaseStatusRefunded    = "refunded"
	PurchaseStatusChargedBack = "charged_back"
)

const (
	LedgerReasonPurchase   = "purchase"
	LedgerReasonRefund     = "refund"
	LedgerReasonChargeback = "chargeback"
)

type PointPurchase struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	PackageID   string
	Points      int
	AmountCents int64
	// Empty until the Mercado Pago order is created.
	MPOrderID         string
	Status            string
	StatusDetail      string
	PointsUnrecovered int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type LedgerEntry struct {
	ID        int64
	UserID    uuid.UUID
	Delta     int64
	Reason    string
	RefID     string
	CreatedAt time.Time
}
