package mercadopago

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mercadopago/sdk-go/pkg/chargeback"
	"github.com/mercadopago/sdk-go/pkg/config"
	"github.com/mercadopago/sdk-go/pkg/invoice"
	"github.com/mercadopago/sdk-go/pkg/mperror"
	"github.com/mercadopago/sdk-go/pkg/order"
	"github.com/mercadopago/sdk-go/pkg/payment"
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
	// Page size for the authorized payments search, and a cap on pages so a
	// misbehaving API can't keep us looping. MP answers "Invalid value for
	// limit" to 50 on this search (its own default is 12), so pages are small.
	chargesPageSize = 10
	chargesMaxPages = 100
)

// Client adapts the Mercado Pago SDK to the PaymentGateway port. API errors
// come back wrapped; use errors.As with *mperror.ResponseError to inspect them.
// A 404 also matches service.ErrNotFound.
type Client struct {
	orders        order.Client
	subscriptions preapproval.Client
	charges       invoice.Client
	payments      payment.Client
	chargebacks   chargeback.Client
}

func NewClient(accessToken string, opts ...config.Option) (*Client, error) {
	cfg, err := config.New(accessToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("mercadopago: %w", err)
	}
	return &Client{
		orders:        order.NewClient(cfg),
		subscriptions: preapproval.NewClient(cfg),
		charges:       invoice.NewClient(cfg),
		payments:      payment.NewClient(cfg),
		chargebacks:   chargeback.NewClient(cfg),
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
			// The Orders API rejects "id" (the SDK has it, but MP answers
			// "additionalProperties 'id' not allowed"); external_code is its
			// item identifier.
			ExternalCode: req.ItemID,
			Title:        req.Title,
			Description:  req.Description,
			CategoryID:   pointsCategoryID,
			UnitPrice:    amount,
			Quantity:     1,
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

func (c *Client) GetPointsOrder(ctx context.Context, orderID string) (model.PointsOrder, error) {
	if orderID == "" {
		return model.PointsOrder{}, errors.New("mercadopago: order id is required")
	}
	resp, err := c.orders.Get(ctx, orderID)
	if err != nil {
		return model.PointsOrder{}, apiError("get order", err)
	}
	amount, err := parseCents(resp.TotalAmount)
	if err != nil {
		return model.PointsOrder{}, fmt.Errorf("mercadopago: order %s: %w", orderID, err)
	}
	return model.PointsOrder{
		ID:                resp.ID,
		Status:            resp.Status,
		StatusDetail:      resp.StatusDetail,
		ExternalReference: resp.ExternalReference,
		AmountCents:       amount,
		CheckoutURL:       resp.CheckoutURL,
	}, nil
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

func (c *Client) GetSubscription(ctx context.Context, subscriptionID string) (model.Subscription, error) {
	if subscriptionID == "" {
		return model.Subscription{}, errors.New("mercadopago: subscription id is required")
	}
	resp, err := c.subscriptions.Get(ctx, subscriptionID)
	if err != nil {
		return model.Subscription{}, apiError("get preapproval", err)
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

func (c *Client) GetSubscriptionCharge(ctx context.Context, chargeID string) (model.Charge, error) {
	if chargeID == "" {
		return model.Charge{}, errors.New("mercadopago: charge id is required")
	}
	resp, err := c.charges.Get(ctx, chargeID)
	if err != nil {
		return model.Charge{}, apiError("get authorized payment", err)
	}
	return toCharge(resp), nil
}

// ListSubscriptionCharges returns every charge MP has for the subscription,
// following the search pagination.
func (c *Client) ListSubscriptionCharges(ctx context.Context, subscriptionID string) ([]model.Charge, error) {
	if subscriptionID == "" {
		return nil, errors.New("mercadopago: subscription id is required")
	}
	var charges []model.Charge
	for page := 0; page < chargesMaxPages; page++ {
		resp, err := c.charges.Search(ctx, invoice.SearchRequest{
			Limit:   chargesPageSize,
			Offset:  page * chargesPageSize,
			Filters: map[string]string{"preapproval_id": subscriptionID},
		})
		if err != nil {
			return nil, apiError("search authorized payments", err)
		}
		for i := range resp.Results {
			charges = append(charges, toCharge(&resp.Results[i]))
		}
		if len(resp.Results) < chargesPageSize || len(charges) >= resp.Paging.Total {
			return charges, nil
		}
	}
	return nil, fmt.Errorf("mercadopago: subscription %s has more than %d charges", subscriptionID, chargesPageSize*chargesMaxPages)
}

func (c *Client) GetPayment(ctx context.Context, paymentID string) (model.Payment, error) {
	id, err := strconv.Atoi(paymentID)
	if err != nil {
		return model.Payment{}, fmt.Errorf("mercadopago: invalid payment id %q", paymentID)
	}
	resp, err := c.payments.Get(ctx, id)
	if err != nil {
		return model.Payment{}, apiError("get payment", err)
	}
	return model.Payment{
		ID:                strconv.Itoa(resp.ID),
		Status:            resp.Status,
		StatusDetail:      resp.StatusDetail,
		ExternalReference: resp.ExternalReference,
		AmountCents:       floatToCents(resp.TransactionAmount),
	}, nil
}

func (c *Client) GetChargebackPaymentID(ctx context.Context, chargebackID string) (string, error) {
	if chargebackID == "" {
		return "", errors.New("mercadopago: chargeback id is required")
	}
	resp, err := c.chargebacks.Get(ctx, chargebackID)
	if err != nil {
		return "", apiError("get chargeback", err)
	}
	if resp.PaymentID == 0 {
		return "", fmt.Errorf("mercadopago: chargeback %s has no payment", chargebackID)
	}
	return strconv.Itoa(resp.PaymentID), nil
}

func toCharge(r *invoice.Response) model.Charge {
	c := model.Charge{
		AuthorizedPaymentID: strconv.Itoa(r.ID),
		PreapprovalID:       r.PreapprovalID,
		Status:              r.Status,
		PaymentStatus:       r.Payment.Status,
		AmountCents:         floatToCents(r.TransactionAmount),
		RetryAttempt:        r.RetryAttempt,
	}
	if r.Payment.ID != 0 {
		c.PaymentID = strconv.Itoa(r.Payment.ID)
	}
	if !r.DebitDate.IsZero() {
		d := r.DebitDate
		c.DebitDate = &d
	}
	return c
}

// apiError wraps an SDK error, adding service.ErrNotFound on a 404 so callers
// don't need to know about mperror.
func apiError(op string, err error) error {
	var notFound *mperror.NotFoundError
	if errors.As(err, &notFound) {
		return fmt.Errorf("mercadopago: %s: %w: %w", op, service.ErrNotFound, err)
	}
	return fmt.Errorf("mercadopago: %s: %w", op, err)
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

// parseCents reads the decimal strings the Orders API uses ("1500.5",
// "1500.50", "1500") without going through float.
func parseCents(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	frac += strings.Repeat("0", 2-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	if strings.HasPrefix(whole, "-") {
		return w*100 - f, nil
	}
	return w*100 + f, nil
}

// floatToCents is for the older APIs that send amounts as JSON numbers.
func floatToCents(v float64) int64 {
	return int64(math.Round(v * 100))
}
