package mercadopago

import (
	"context"
	"errors"
	"fmt"

	"github.com/mercadopago/sdk-go/pkg/config"
	"github.com/mercadopago/sdk-go/pkg/order"
	"github.com/mercadopago/sdk-go/pkg/preapproval"
	"github.com/mercadopago/sdk-go/pkg/requestoptions"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/service"
)

var _ service.PaymentGateway = (*Client)(nil)

const (
	// Shown on the buyer's card statement; a recognizable name lowers chargebacks.
	statementDescriptor = "DISCORDIA"
	// Points are a digital good; MP uses the category for fraud prevention.
	pointsCategoryID = "virtual_goods"
)

// Client adapts the Mercado Pago SDK to the PaymentGateway port. API errors
// come back wrapped; use errors.As with *mperror.ResponseError to inspect them.
type Client struct {
	orders        order.Client
	subscriptions preapproval.Client
}

func NewClient(accessToken string, opts ...config.Option) (*Client, error) {
	cfg, err := config.New(accessToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("mercadopago: %w", err)
	}
	return &Client{
		orders:        order.NewClient(cfg),
		subscriptions: preapproval.NewClient(cfg),
	}, nil
}

func (c *Client) CreatePointsOrder(ctx context.Context, req model.PointsOrderRequest) (model.PointsOrder, error) {
	if req.IdempotencyKey == "" {
		return model.PointsOrder{}, errors.New("mercadopago: idempotency key is required")
	}
	amount := formatCents(req.AmountCents)
	body := order.Request{
		// Checkout Pro only accepts type "online" with processing_mode "manual".
		Type:              "online",
		ProcessingMode:    "manual",
		TotalAmount:       amount,
		ExternalReference: req.ExternalReference,
		Items: []order.ItemsRequest{{
			ID:          req.ItemID,
			Title:       req.Title,
			Description: req.Description,
			CategoryID:  pointsCategoryID,
			UnitPrice:   amount,
			Quantity:    1,
		}},
		Config: &order.ConfigRequest{StatementDescriptor: statementDescriptor},
	}
	if req.PayerEmail != "" || req.PayerFirstName != "" || req.PayerLastName != "" {
		body.Payer = &order.PayerRequest{
			Email:     req.PayerEmail,
			FirstName: req.PayerFirstName,
			LastName:  req.PayerLastName,
		}
	}
	if req.SuccessURL != "" {
		// "all" brings the buyer back on any outcome: the return page polls
		// billing for the real status instead of trusting the redirect.
		body.Config.Online = &order.OnlineConfigRequest{
			SuccessURL: req.SuccessURL,
			FailureURL: req.FailureURL,
			PendingURL: req.PendingURL,
			AutoReturn: "all",
		}
	}

	ctx = requestoptions.WithIdempotencyKey(ctx, req.IdempotencyKey)
	resp, err := c.orders.Create(ctx, body)
	if err != nil {
		return model.PointsOrder{}, fmt.Errorf("mercadopago: create order: %w", err)
	}
	return model.PointsOrder{ID: resp.ID, Status: resp.Status, CheckoutURL: resp.CheckoutURL}, nil
}

func (c *Client) CreateSubscription(ctx context.Context, req model.SubscriptionRequest) (model.Subscription, error) {
	resp, err := c.subscriptions.Create(ctx, preapproval.Request{
		Reason:            req.Reason,
		ExternalReference: req.ExternalReference,
		PayerEmail:        req.PayerEmail,
		BackURL:           req.BackURL,
		Status:            model.SubscriptionStatusPending,
		AutoRecurring: &preapproval.AutoRecurringRequest{
			Frequency:         1,
			FrequencyType:     "months",
			TransactionAmount: float64(req.AmountCents) / 100,
			CurrencyID:        req.CurrencyID,
		},
	})
	if err != nil {
		return model.Subscription{}, fmt.Errorf("mercadopago: create preapproval: %w", err)
	}
	return toSubscription(resp), nil
}

func (c *Client) CancelSubscription(ctx context.Context, subscriptionID string) (model.Subscription, error) {
	if subscriptionID == "" {
		return model.Subscription{}, errors.New("mercadopago: subscription id is required")
	}
	resp, err := c.subscriptions.Update(ctx, subscriptionID, preapproval.UpdateRequest{
		Status: model.SubscriptionStatusCancelled,
	})
	if err != nil {
		return model.Subscription{}, fmt.Errorf("mercadopago: cancel preapproval: %w", err)
	}
	return toSubscription(resp), nil
}

func toSubscription(r *preapproval.Response) model.Subscription {
	return model.Subscription{
		ID:                r.ID,
		Status:            r.Status,
		ExternalReference: r.ExternalReference,
		InitPoint:         r.InitPoint,
	}
}

func formatCents(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
