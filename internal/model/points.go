package model

import (
	"time"

	"github.com/google/uuid"
)

type PurchaseStatus string

const (
	PurchaseStatusPending     PurchaseStatus = "pending"
	PurchaseStatusPaid        PurchaseStatus = "paid"
	PurchaseStatusFailed      PurchaseStatus = "failed"
	PurchaseStatusExpired     PurchaseStatus = "expired"
	PurchaseStatusRefunded    PurchaseStatus = "refunded"
	PurchaseStatusChargedBack PurchaseStatus = "charged_back"
)

type LedgerReason string

const (
	LedgerReasonPurchase   LedgerReason = "purchase"
	LedgerReasonRefund     LedgerReason = "refund"
	LedgerReasonChargeback LedgerReason = "chargeback"
)

type PointPurchase struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	PackageID   string
	Points      int
	AmountCents int64
	// Empty until the Mercado Pago order is created.
	MPOrderID         string
	Status            PurchaseStatus
	StatusDetail      string
	PointsUnrecovered int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type LedgerEntry struct {
	ID        int64
	UserID    uuid.UUID
	Delta     int64
	Reason    LedgerReason
	RefID     string
	CreatedAt time.Time
}
