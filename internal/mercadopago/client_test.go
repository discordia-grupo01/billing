package mercadopago

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mercadopago/sdk-go/pkg/config"
	"github.com/mercadopago/sdk-go/pkg/mperror"

	"github.com/discordia-grupo01/billing/internal/model"
)

// redirectRequester sends the SDK's requests (hardcoded to api.mercadopago.com)
// to the test server instead.
type redirectRequester struct{ target *url.URL }

func (r redirectRequester) Do(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return http.DefaultClient.Do(req)
}

type captured struct {
	method  string
	path    string
	headers http.Header
	body    map[string]any
}

func newServer(t *testing.T, status int, response string) (*Client, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.headers = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	client, err := NewClient("TEST-TOKEN", config.WithHTTPClient(redirectRequester{target}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, got
}

func TestCreatePointsOrder(t *testing.T) {
	client, got := newServer(t, http.StatusCreated,
		`{"id":"ORD123","status":"created","checkout_url":"https://mp.test/checkout?order_id=ORD123"}`)

	order, err := client.CreatePointsOrder(context.Background(), model.PointsOrderRequest{
		IdempotencyKey:    "key-1",
		ExternalReference: "purchase-42",
		PayerEmail:        "buyer@test.com",
		PayerFirstName:    "Ana",
		PayerLastName:     "Pérez",
		ItemID:            "points_500",
		Title:             "500 puntos",
		Description:       "500 puntos de Discordia",
		AmountCents:       150050,
		SuccessURL:        "https://discordia.test/billing/return?r=success",
		FailureURL:        "https://discordia.test/billing/return?r=failure",
		PendingURL:        "https://discordia.test/billing/return?r=pending",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if order.ID != "ORD123" || order.Status != "created" || order.CheckoutURL != "https://mp.test/checkout?order_id=ORD123" {
		t.Errorf("unexpected order: %+v", order)
	}
	if got.method != http.MethodPost || got.path != "/v1/orders" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if h := got.headers.Get("Authorization"); h != "Bearer TEST-TOKEN" {
		t.Errorf("Authorization = %q", h)
	}
	if h := got.headers.Get("X-Idempotency-Key"); h != "key-1" {
		t.Errorf("X-Idempotency-Key = %q", h)
	}
	if got.body["type"] != "online" || got.body["processing_mode"] != "manual" {
		t.Errorf("checkout pro fields wrong: %v", got.body)
	}
	if got.body["total_amount"] != "1500.50" {
		t.Errorf("total_amount = %v", got.body["total_amount"])
	}
	if got.body["external_reference"] != "purchase-42" {
		t.Errorf("external_reference = %v", got.body["external_reference"])
	}
	items, _ := got.body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", got.body["items"])
	}
	item := items[0].(map[string]any)
	if item["unit_price"] != "1500.50" || item["quantity"] != 1.0 || item["id"] != "points_500" ||
		item["description"] != "500 puntos de Discordia" || item["category_id"] != "virtual_goods" {
		t.Errorf("item = %v", item)
	}
	payer, _ := got.body["payer"].(map[string]any)
	if payer["email"] != "buyer@test.com" || payer["first_name"] != "Ana" || payer["last_name"] != "Pérez" {
		t.Errorf("payer = %v", got.body["payer"])
	}
	cfg, _ := got.body["config"].(map[string]any)
	if cfg["statement_descriptor"] != "DISCORDIA" {
		t.Errorf("statement_descriptor = %v", cfg["statement_descriptor"])
	}
	online, _ := cfg["online"].(map[string]any)
	if online["success_url"] != "https://discordia.test/billing/return?r=success" ||
		online["failure_url"] != "https://discordia.test/billing/return?r=failure" ||
		online["pending_url"] != "https://discordia.test/billing/return?r=pending" ||
		online["auto_return"] != "all" {
		t.Errorf("config.online = %v", online)
	}
}

func TestCreatePointsOrderOmitsOptionalBlocks(t *testing.T) {
	client, got := newServer(t, http.StatusCreated, `{"id":"ORD1"}`)

	_, err := client.CreatePointsOrder(context.Background(), model.PointsOrderRequest{
		IdempotencyKey: "k", Title: "x", AmountCents: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got.body["payer"]; ok {
		t.Errorf("payer should be omitted, got %v", got.body["payer"])
	}
	cfg, _ := got.body["config"].(map[string]any)
	if _, ok := cfg["online"]; ok {
		t.Errorf("config.online should be omitted without back URLs, got %v", cfg["online"])
	}
}

func TestCreatePointsOrderRequiresIdempotencyKey(t *testing.T) {
	client, got := newServer(t, http.StatusCreated, `{}`)

	_, err := client.CreatePointsOrder(context.Background(), model.PointsOrderRequest{AmountCents: 100})
	if err == nil {
		t.Fatal("expected error")
	}
	if got.method != "" {
		t.Error("request should not be sent without idempotency key")
	}
}

func TestCreateSubscription(t *testing.T) {
	client, got := newServer(t, http.StatusCreated,
		`{"id":"PRE1","status":"pending","external_reference":"user-7","init_point":"https://mp.test/subscribe"}`)

	sub, err := client.CreateSubscription(context.Background(), model.SubscriptionRequest{
		ExternalReference: "user-7",
		PayerEmail:        "buyer@test.com",
		Reason:            "Discordia Premium",
		AmountCents:       499900,
		CurrencyID:        "ARS",
		BackURL:           "https://discordia.test/billing/return",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := model.Subscription{ID: "PRE1", Status: "pending", ExternalReference: "user-7", InitPoint: "https://mp.test/subscribe"}
	if sub != want {
		t.Errorf("got %+v, want %+v", sub, want)
	}
	if got.method != http.MethodPost || got.path != "/preapproval" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if got.body["status"] != "pending" || got.body["payer_email"] != "buyer@test.com" {
		t.Errorf("unexpected body: %v", got.body)
	}
	recurring, _ := got.body["auto_recurring"].(map[string]any)
	if recurring["transaction_amount"] != 4999.0 || recurring["currency_id"] != "ARS" ||
		recurring["frequency"] != 1.0 || recurring["frequency_type"] != "months" {
		t.Errorf("auto_recurring = %v", recurring)
	}
}

func TestCancelSubscription(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{"id":"PRE1","status":"cancelled","external_reference":"user-7"}`)

	sub, err := client.CancelSubscription(context.Background(), "PRE1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.Status != model.SubscriptionStatusCancelled {
		t.Errorf("status = %q", sub.Status)
	}
	if got.method != http.MethodPut || got.path != "/preapproval/PRE1" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if got.body["status"] != "cancelled" {
		t.Errorf("status sent = %v, MP only accepts \"cancelled\"", got.body["status"])
	}
}

func TestCancelSubscriptionRequiresID(t *testing.T) {
	client, _ := newServer(t, http.StatusOK, `{}`)

	if _, err := client.CancelSubscription(context.Background(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestAPIErrorIsExposed(t *testing.T) {
	client, _ := newServer(t, http.StatusBadRequest, `{"message":"Invalid preapproval status param: canceled","status":400}`)

	_, err := client.CancelSubscription(context.Background(), "PRE1")

	var apiErr *mperror.ResponseError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *mperror.ResponseError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
}

func TestCreateErrorsAreWrapped(t *testing.T) {
	client, _ := newServer(t, http.StatusUnauthorized, `{"message":"Unauthorized access to resource.","status":401}`)

	if _, err := client.CreatePointsOrder(context.Background(), model.PointsOrderRequest{IdempotencyKey: "k", AmountCents: 100}); err == nil {
		t.Error("CreatePointsOrder: expected error")
	}
	if _, err := client.CreateSubscription(context.Background(), model.SubscriptionRequest{AmountCents: 100}); err == nil {
		t.Error("CreateSubscription: expected error")
	}
}

func TestDecodeError(t *testing.T) {
	client, _ := newServer(t, http.StatusOK, `not json`)

	if _, err := client.CancelSubscription(context.Background(), "PRE1"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestFormatCents(t *testing.T) {
	cases := map[int64]string{0: "0.00", 5: "0.05", 100: "1.00", 150050: "1500.50", -250: "-2.50"}
	for in, want := range cases {
		if got := formatCents(in); got != want {
			t.Errorf("formatCents(%d) = %q, want %q", in, got, want)
		}
	}
}
