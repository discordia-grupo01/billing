package service

import (
	"context"

	"github.com/discordia-grupo01/billing/internal/model"
)

// PaymentGateway is the port to the payment provider. Business logic depends
// on this interface only; internal/mercadopago is the real implementation.
type PaymentGateway interface {
	CreatePointsOrder(ctx context.Context, req model.PointsOrderRequest) (model.PointsOrder, error)
	CreateSubscription(ctx context.Context, req model.SubscriptionRequest) (model.Subscription, error)
	CancelSubscription(ctx context.Context, subscriptionID string) (model.Subscription, error)
}
