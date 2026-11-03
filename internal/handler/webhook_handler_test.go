package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/service"
)

const webhookSecret = "whsec"

var webhookNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// recorder stands in for both services and records which use case ran.
type recorder struct {
	calls []string
	err   error
}

func (r *recorder) record(name, id string) error {
	r.calls = append(r.calls, name+":"+id)
	return r.err
}

func (r *recorder) ProcessOrderNotification(_ context.Context, id string) error {
	return r.record("order", id)
}

func (r *recorder) ProcessSubscriptionNotification(_ context.Context, id string) error {
	return r.record("subscription", id)
}

func (r *recorder) ProcessChargeNotification(_ context.Context, id string) error {
	return r.record("charge", id)
}

func (r *recorder) ProcessPaymentNotification(_ context.Context, id string) error {
	return r.record("payment", id)
}

func (r *recorder) ProcessChargebackNotification(_ context.Context, id string) error {
	return r.record("chargeback", id)
}

func newWebhookRouter(rec *recorder) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewWebhookHandler(rec, rec, webhookSecret, quietLog)
	h.now = func() time.Time { return webhookNow }
	r := gin.New()
	h.Register(r)
	return r
}

// signature builds x-signature the way MP does: HMAC-SHA256 of
// "id:<lowercased data.id>;request-id:<x-request-id>;ts:<ts>;", ts in ms.
func signature(secret, dataID, requestID string, at time.Time) string {
	ts := strconv.FormatInt(at.UnixMilli(), 10)
	manifest := ""
	if dataID != "" {
		manifest += "id:" + strings.ToLower(dataID) + ";"
	}
	manifest += "request-id:" + requestID + ";ts:" + ts + ";"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(manifest))
	return "ts=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

type delivery struct {
	topic, queryID, body string
	sig                  string // empty: a valid signature for queryID
}

func send(r http.Handler, d delivery) *httptest.ResponseRecorder {
	q := url.Values{}
	if d.topic != "" {
		q.Set("type", d.topic)
	}
	if d.queryID != "" {
		q.Set("data.id", d.queryID)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/mercadopago?"+q.Encode(), strings.NewReader(d.body))
	req.Header.Set("x-request-id", "req-1")
	sig := d.sig
	if sig == "" {
		sig = signature(webhookSecret, d.queryID, "req-1", webhookNow)
	}
	req.Header.Set("x-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWebhookRoutesTopics(t *testing.T) {
	tests := map[string]string{
		"order":                           "order:ORD01JQ4S4KY8HWQ6NA5PXB65B3D3",
		"subscription_preapproval":        "subscription:ORD01JQ4S4KY8HWQ6NA5PXB65B3D3",
		"subscription_authorized_payment": "charge:ORD01JQ4S4KY8HWQ6NA5PXB65B3D3",
		"payment":                         "payment:ORD01JQ4S4KY8HWQ6NA5PXB65B3D3",
		"topic_chargebacks_wh":            "chargeback:ORD01JQ4S4KY8HWQ6NA5PXB65B3D3",
		"chargebacks":                     "chargeback:ORD01JQ4S4KY8HWQ6NA5PXB65B3D3",
	}
	for topic, want := range tests {
		t.Run(topic, func(t *testing.T) {
			rec := &recorder{}
			// Uppercase id: MP signs it lowercased, but the use case gets it as sent.
			w := send(newWebhookRouter(rec), delivery{topic: topic, queryID: "ORD01JQ4S4KY8HWQ6NA5PXB65B3D3"})

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			if len(rec.calls) != 1 || rec.calls[0] != want {
				t.Errorf("calls = %v, want [%s]", rec.calls, want)
			}
		})
	}
}

func TestWebhookTopicFromBody(t *testing.T) {
	rec := &recorder{}
	w := send(newWebhookRouter(rec), delivery{queryID: "123", body: `{"type":"payment","data":{"id":"123"}}`})

	if w.Code != http.StatusOK || len(rec.calls) != 1 || rec.calls[0] != "payment:123" {
		t.Errorf("status %d, calls %v", w.Code, rec.calls)
	}
}

func TestWebhookTopicFromLegacyQuery(t *testing.T) {
	rec := &recorder{}
	r := newWebhookRouter(rec)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/mercadopago?topic=payment&data.id=9", nil)
	req.Header.Set("x-request-id", "req-1")
	req.Header.Set("x-signature", signature(webhookSecret, "9", "req-1", webhookNow))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK || len(rec.calls) != 1 || rec.calls[0] != "payment:9" {
		t.Errorf("status %d, calls %v", w.Code, rec.calls)
	}
}

func TestWebhookIDFromBodyWhenQueryHasNone(t *testing.T) {
	for name, body := range map[string]string{
		"string id": `{"type":"payment","data":{"id":"456"}}`,
		"number id": `{"type":"payment","data":{"id":456}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			w := send(newWebhookRouter(rec), delivery{body: body})

			if w.Code != http.StatusOK || len(rec.calls) != 1 || rec.calls[0] != "payment:456" {
				t.Errorf("status %d, calls %v", w.Code, rec.calls)
			}
		})
	}
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	tests := map[string]string{
		"missing":       " ",
		"malformed":     "garbage",
		"wrong secret":  signature("other", "123", "req-1", webhookNow),
		"other id":      signature(webhookSecret, "999", "req-1", webhookNow),
		"too old":       signature(webhookSecret, "123", "req-1", webhookNow.Add(-6*time.Minute)),
		"too far ahead": signature(webhookSecret, "123", "req-1", webhookNow.Add(6*time.Minute)),
	}
	for name, sig := range tests {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			w := send(newWebhookRouter(rec), delivery{topic: "payment", queryID: "123", sig: sig})

			assertError(t, w, http.StatusUnauthorized, CodeInvalidSignature)
			if len(rec.calls) != 0 {
				t.Errorf("nothing should be processed: %v", rec.calls)
			}
		})
	}
}

// The panel's simulated notifications sign ts in seconds, not milliseconds.
func TestWebhookAcceptsTimestampInSeconds(t *testing.T) {
	sign := func(at time.Time) string {
		ts := strconv.FormatInt(at.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(webhookSecret))
		mac.Write([]byte("id:123;request-id:req-1;ts:" + ts + ";"))
		return "ts=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	}
	tests := map[string]struct {
		at   time.Time
		want int
	}{
		"now":          {webhookNow, http.StatusOK},
		"4 min ago":    {webhookNow.Add(-4 * time.Minute), http.StatusOK},
		"10 min ago":   {webhookNow.Add(-10 * time.Minute), http.StatusUnauthorized},
		"10 min ahead": {webhookNow.Add(10 * time.Minute), http.StatusUnauthorized},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			w := send(newWebhookRouter(rec), delivery{topic: "payment", queryID: "123", sig: sign(tc.at)})
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestWebhookAcceptsSignatureWithinTolerance(t *testing.T) {
	rec := &recorder{}
	sig := signature(webhookSecret, "123", "req-1", webhookNow.Add(-4*time.Minute))
	w := send(newWebhookRouter(rec), delivery{topic: "payment", queryID: "123", sig: sig})

	if w.Code != http.StatusOK || len(rec.calls) != 1 {
		t.Errorf("status %d, calls %v", w.Code, rec.calls)
	}
}

func TestWebhookIgnored(t *testing.T) {
	tests := map[string]delivery{
		"unhandled topic": {topic: "merchant_order", queryID: "1"},
		"no id":           {topic: "payment"},
		"invalid JSON":    {topic: "payment", body: "{"},
	}
	for name, d := range tests {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			w := send(newWebhookRouter(rec), d)

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 so MP stops retrying", w.Code)
			}
			if len(rec.calls) != 0 {
				t.Errorf("nothing should be processed: %v", rec.calls)
			}
		})
	}
}

func TestWebhookAcknowledgesMismatches(t *testing.T) {
	for _, err := range []error{service.ErrOrderMismatch, service.ErrSubscriptionMismatch} {
		t.Run(err.Error(), func(t *testing.T) {
			rec := &recorder{err: fmt.Errorf("wrapped: %w", err)}
			w := send(newWebhookRouter(rec), delivery{topic: "order", queryID: "ORD1"})

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: retrying won't fix a mismatch", w.Code)
			}
		})
	}
}

func TestWebhookFailureAsksForRetry(t *testing.T) {
	rec := &recorder{err: errBoom}
	w := send(newWebhookRouter(rec), delivery{topic: "order", queryID: "ORD1"})

	assertError(t, w, http.StatusInternalServerError, CodeInternalError)
}

func TestWebhookRejectsHugeBody(t *testing.T) {
	rec := &recorder{}
	w := send(newWebhookRouter(rec), delivery{topic: "order", queryID: "ORD1", body: strings.Repeat("x", maxWebhookBody+1)})

	if w.Code != http.StatusRequestEntityTooLarge || len(rec.calls) != 0 {
		t.Errorf("status %d, calls %v", w.Code, rec.calls)
	}
}

func TestNewWebhookHandlerDefaultsLogger(t *testing.T) {
	if h := NewWebhookHandler(&recorder{}, &recorder{}, "s", nil); h.log == nil {
		t.Error("logger should default to slog.Default")
	}
}
