package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mercadopago/sdk-go/pkg/webhook"

	"github.com/discordia-grupo01/billing/internal/service"
)

const (
	// MP retries anything not answered with 2xx within 22 s.
	webhookTimeout = 20 * time.Second
	// MP signs ts; older notifications are treated as replays.
	signatureTolerance = 5 * time.Minute
	maxWebhookBody     = 64 << 10
)

// Topics configured in the MP panel (Tus integraciones > Webhooks).
const (
	topicOrder              = "order"
	topicSubscription       = "subscription_preapproval"
	topicSubscriptionCharge = "subscription_authorized_payment"
	topicPayment            = "payment"
	topicChargeback         = "topic_chargebacks_wh"
	topicChargebackShort    = "chargebacks"
)

type OrderNotifications interface {
	ProcessOrderNotification(ctx context.Context, orderID string) error
}

type PremiumNotifications interface {
	ProcessSubscriptionNotification(ctx context.Context, preapprovalID string) error
	ProcessChargeNotification(ctx context.Context, chargeID string) error
	ProcessPaymentNotification(ctx context.Context, paymentID string) error
	ProcessChargebackNotification(ctx context.Context, chargebackID string) error
}

// WebhookHandler receives Mercado Pago notifications. It only trusts the
// signature: the body just says "resource X changed", and every use case
// re-fetches X from MP and is safe to run again.
type WebhookHandler struct {
	orders  OrderNotifications
	premium PremiumNotifications
	secret  string
	log     *slog.Logger
	now     func() time.Time
}

func NewWebhookHandler(orders OrderNotifications, premium PremiumNotifications, secret string, logger *slog.Logger) *WebhookHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookHandler{orders: orders, premium: premium, secret: secret, log: logger, now: time.Now}
}

func (h *WebhookHandler) Register(r gin.IRouter) {
	r.POST("/webhooks/mercadopago", h.receive)
}

type notification struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	Data   struct {
		ID flexibleID `json:"id"`
	} `json:"data"`
}

// flexibleID accepts ids sent as strings or as numbers: some topics use one,
// some the other.
type flexibleID string

func (f *flexibleID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexibleID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexibleID(n.String())
	return nil
}

func (h *WebhookHandler) receive(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBody))
	if err != nil {
		writeError(c, http.StatusRequestEntityTooLarge, CodeInvalidRequest, invalidRequestMessage)
		return
	}
	requestID := c.GetHeader("x-request-id")
	// The signature covers data.id from the query string, not the body.
	queryID := c.Query("data.id")

	// MP signs the id lowercased. The SDK's docs say it lowercases it, but
	// v1.14.0 doesn't, and order ids (ORD01...) are uppercase.
	err = webhook.ValidateSignature(c.GetHeader("x-signature"), requestID, strings.ToLower(queryID), h.secret,
		webhook.WithTolerance(signatureTolerance), webhook.WithNow(h.now))
	if err != nil {
		var sigErr *webhook.SignatureError
		reason := ""
		if errors.As(err, &sigErr) {
			reason = string(sigErr.Reason)
		}
		h.log.Warn("rejected webhook with invalid signature", "reason", reason, "request_id", requestID)
		writeError(c, http.StatusUnauthorized, CodeInvalidSignature, invalidSignatureMessage)
		return
	}

	var n notification
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &n); err != nil {
			h.log.Warn("webhook body is not valid JSON", "request_id", requestID, "error", err)
		}
	}
	topic := firstNonEmpty(c.Query("type"), c.Query("topic"), n.Type)
	id := queryID
	if id == "" {
		id = string(n.Data.ID)
		if id != "" {
			h.log.Warn("webhook without data.id in the query, using the body", "topic", topic, "id", id, "request_id", requestID)
		}
	}
	log := h.log.With("topic", topic, "id", id, "action", n.Action, "request_id", requestID)
	if id == "" {
		log.Warn("ignoring webhook without resource id")
		c.Status(http.StatusOK)
		return
	}

	process := h.processor(topic)
	if process == nil {
		log.Info("ignoring webhook for unhandled topic")
		c.Status(http.StatusOK)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), webhookTimeout)
	defer cancel()
	switch err := process(ctx, id); {
	case err == nil:
		c.Status(http.StatusOK)
	case errors.Is(err, service.ErrOrderMismatch), errors.Is(err, service.ErrSubscriptionMismatch):
		// Retrying won't fix it: acknowledge and leave it for a human.
		log.Error("webhook resource does not match our records", "error", err)
		c.Status(http.StatusOK)
	default:
		// MP retries later; every use case is safe to run again.
		log.Error("webhook processing failed", "error", err)
		writeError(c, http.StatusInternalServerError, CodeInternalError, internalErrorMessage)
	}
}

func (h *WebhookHandler) processor(topic string) func(context.Context, string) error {
	switch topic {
	case topicOrder:
		return h.orders.ProcessOrderNotification
	case topicSubscription:
		return h.premium.ProcessSubscriptionNotification
	case topicSubscriptionCharge:
		return h.premium.ProcessChargeNotification
	case topicPayment:
		return h.premium.ProcessPaymentNotification
	case topicChargeback, topicChargebackShort:
		return h.premium.ProcessChargebackNotification
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
