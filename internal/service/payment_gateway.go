package service

import (
	"context"
	"errors"

	"github.com/discordia-grupo01/billing/internal/model"
)

// ErrNotFound means the provider doesn't know the resource, e.g. a webhook
// test notification with a fake id. Retrying won't help.
var ErrNotFound = errors.New("resource not found at payment provider")

// PaymentGateway is the port to the payment provider. Business logic depends
// on this interface only; internal/mercadopago is the real implementation.
type PaymentGateway interface {
	CreatePointsOrder(ctx context.Context, req model.PointsOrderRequest) (model.PointsOrder, error)
	GetPointsOrder(ctx context.Context, orderID string) (model.PointsOrder, error)

	CreateSubscription(ctx context.Context, req model.SubscriptionRequest) (model.Subscription, error)
	GetSubscription(ctx context.Context, subscriptionID string) (model.Subscription, error)
	CancelSubscription(ctx context.Context, subscriptionID string) (model.Subscription, error)

	GetSubscriptionCharge(ctx context.Context, chargeID string) (model.Charge, error)
	ListSubscriptionCharges(ctx context.Context, subscriptionID string) ([]model.Charge, error)

	GetPayment(ctx context.Context, paymentID string) (model.Payment, error)
	// GetChargebackPaymentID returns the id of the payment a chargeback disputes.
	GetChargebackPaymentID(ctx context.Context, chargebackID string) (string, error)
}
